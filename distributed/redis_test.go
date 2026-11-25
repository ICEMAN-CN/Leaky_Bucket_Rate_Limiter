package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) string {
	t.Helper()
	if address := os.Getenv("REDIS_TEST_ADDR"); address != "" {
		return address
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("redis-server", "--bind", "127.0.0.1", "--port", port,
		"--save", "", "--appendonly", "no", "--dir", t.TempDir())
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("local redis-server or REDIS_TEST_ADDR is required: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		if err := cmd.Wait(); err != nil {
			t.Logf("test Redis process exited: %v, %s", err, output.String())
		}
	})
	client := newRedisStore(address, 5, 1)
	defer client.client.Close()
	until := time.Now().Add(5 * time.Second)
	for {
		if err := client.ready(t.Context()); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("Redis startup timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return address
}

func testStore(t *testing.T, addr string, capacity int, rate float64) *redisStore {
	s := newRedisStore(addr, capacity, rate)
	t.Cleanup(func() { _ = s.client.Close() })
	return s
}

func testUser(t *testing.T, client *redis.Client) string {
	user := "test-" + rand.Text()
	t.Cleanup(func() { _ = client.Del(context.Background(), bucketKey(user)).Err() })
	return user
}

func TestRedis(t *testing.T) {
	addr := testRedis(t)
	t.Run("shared contract", func(t *testing.T) { checkVectors(t, addr) })
	t.Run("empty bucket expiry", func(t *testing.T) { checkExpiry(t, addr) })
	t.Run("rolling policy update", func(t *testing.T) { checkPolicyChange(t, addr) })
	t.Run("upgrade an active legacy bucket", func(t *testing.T) { checkLegacyPolicy(t, addr) })
	t.Run("atomic admission across two HTTP instances", func(t *testing.T) { checkConcurrent(t, addr) })
	t.Run("storage network interruption and recovery", func(t *testing.T) { checkDisconnect(t, addr) })
}

func checkLegacyPolicy(t *testing.T, addr string) {
	original := testStore(t, addr, 1, 0.01)
	user := testUser(t, original.client)
	if ok, _, err := original.check(t.Context(), user); !ok || err != nil {
		t.Fatal(ok, err)
	}
	// The legacy format records level, time and busy-period count without a policy snapshot.
	if err := original.client.HDel(t.Context(), bucketKey(user),
		"capacity", "leak_rate", "units", "period", "compact").Err(); err != nil {
		t.Fatal(err)
	}
	b, err := original.get(t.Context(), user)
	if err != nil || b == nil || b.Capacity != 1 || b.LeakRate != 0.01 {
		t.Fatal(b, err)
	}
	if ok, _, err := original.check(t.Context(), user); ok || err != nil {
		t.Fatal(ok, err)
	}
	changed := testStore(t, addr, 5, 20)
	b, err = changed.get(t.Context(), user)
	if err != nil || b == nil || b.Capacity != 1 || b.LeakRate != 0.01 {
		t.Fatal(b, err)
	}
}

func checkPolicyChange(t *testing.T, addr string) {
	original := testStore(t, addr, 1, 0.01)
	changed := testStore(t, addr, 5, 20)
	user := testUser(t, original.client)
	if ok, _, err := original.check(t.Context(), user); !ok || err != nil {
		t.Fatal(ok, err)
	}
	before, err := changed.get(t.Context(), user)
	if err != nil || before == nil || before.Capacity != 1 || before.LeakRate != 0.01 {
		t.Fatalf("new replica read legacy bucket %+v, error %v", before, err)
	}
	if ok, b, err := changed.check(t.Context(), user); ok || err != nil || b.Capacity != 1 || b.LeakRate != 0.01 {
		t.Fatalf("busy bucket should retain its original policy, got %+v, allowed %t, error %v", b, ok, err)
	}
	fresh := testUser(t, changed.client)
	if ok, b, err := changed.check(t.Context(), fresh); !ok || err != nil || b.Capacity != 5 || b.LeakRate != 20 {
		t.Fatal(ok, b, err)
	}
	// Wait until the new high-rate bucket drains. The next request should adopt the handling replica's policy.
	until := time.Now().Add(2 * time.Second)
	for {
		b, err := original.get(t.Context(), fresh)
		if err != nil {
			t.Fatal(err)
		}
		if b == nil {
			break
		}
		if b.Capacity != 5 || b.LeakRate != 20 || time.Now().After(until) {
			t.Fatal(b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ok, b, err := original.check(t.Context(), fresh); !ok || err != nil || b.Capacity != 1 || b.LeakRate != 0.01 {
		t.Fatal(ok, b, err)
	}
}

func TestDecimalLeakPeriod(t *testing.T) {
	for _, tc := range []struct {
		rate float64
		want leakPeriod
	}{
		{1, leakPeriod{1, 1_000_000, 1}},
		{0.29, leakPeriod{29, 100_000_000, 1}},
		{3.5, leakPeriod{7, 2_000_000, 1}},
		{1e8, leakPeriod{100, 1, 1}},
	} {
		if got := periodFor(tc.rate); got != tc.want {
			t.Errorf("microsecond schedule for leak rate %g is %+v, want %+v", tc.rate, got, tc.want)
		}
	}
}

func checkVectors(t *testing.T, addr string) {
	file, err := os.ReadFile("../contracts/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string
		Capacity int
		Rate     float64 `json:"leak_rate"`
		Steps    []struct {
			User      string `json:"user_id"`
			Timestamp float64
			Allowed   bool
			Level     float64
			UpdatedAt float64 `json:"updated_at"`
		}
	}
	if err := json.Unmarshal(file, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("empty contract")
	}
	// Inject the time into the pure transition only in tests. The production entry point always calls Redis TIME.
	script := redis.NewScript(advanceScript + `
local allowed, water, at, count, anchor = advance(tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7]), ARGV[8] == '1')
return {allowed and 1 or 0, string.format('%.17g', water), string.format('%.0f', at), count, anchor}`)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			s := testStore(t, addr, c.Capacity, c.Rate)
			type state struct{ count, anchor, last int64 }
			states := make(map[string]state)
			for i, step := range c.Steps {
				previous, ok := states[step.User]
				if !ok {
					previous.anchor = int64(math.Round(step.Timestamp * 1e6))
					previous.last = previous.anchor
				}
				values, err := script.Run(t.Context(), s.client, nil, previous.count, previous.anchor, previous.last,
					math.Round(step.Timestamp*1e6), c.Capacity, s.period.units, s.period.micros, s.period.compact).Slice()
				if err != nil {
					t.Fatal(err)
				}
				b, err := s.decode(values[1:3])
				if err != nil {
					t.Fatal(err)
				}
				if (values[0].(int64) == 1) != step.Allowed || math.Abs(b.Level-step.Level) > 1e-9 || b.UpdatedAt != step.UpdatedAt {
					t.Fatalf("step %d returned %v, want %+v", i, values, step)
				}
				states[step.User] = state{values[3].(int64), values[4].(int64), int64(math.Round(b.UpdatedAt * 1e6))}
			}
		})
	}
}

func checkExpiry(t *testing.T, addr string) {
	s := testStore(t, addr, 5, 0.41)
	user := testUser(t, s.client)
	allowed, b, err := s.check(t.Context(), user)
	if err != nil || !allowed {
		t.Fatal(allowed, err)
	}
	stored, err := s.get(t.Context(), user)
	if err != nil || stored == nil || *stored != b {
		t.Fatal(stored, err)
	}
	// Redis returns an absolute expiry in milliseconds. Rounding down would release quota early.
	expiry, err := s.client.Do(t.Context(), "PEXPIRETIME", bucketKey(user)).Int64()
	if err != nil {
		t.Fatal(err)
	}
	expected := math.Ceil(float64(math.Round(b.UpdatedAt*1e6))/1000 + b.Level/s.rate*1000)
	if expiry != int64(expected) {
		t.Fatalf("expiry %d, want %.0f", expiry, expected)
	}
	fast := testStore(t, addr, 1, 40)
	fastUser := testUser(t, fast.client)
	if ok, _, err := fast.check(t.Context(), fastUser); !ok || err != nil {
		t.Fatal(ok, err)
	}
	until := time.Now().Add(2 * time.Second)
	for {
		value, err := fast.get(t.Context(), fastUser)
		if err != nil {
			t.Fatal(err)
		}
		if value == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("empty bucket did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ok, _, err := fast.check(t.Context(), fastUser); !ok || err != nil {
		t.Fatal(ok, err)
	}
}

func checkConcurrent(t *testing.T, addr string) {
	a, b := testStore(t, addr, 5, 0.001), testStore(t, addr, 5, 0.001)
	servers := []*httptest.Server{httptest.NewServer(newAPI(a, "a")), httptest.NewServer(newAPI(b, "b"))}
	for _, server := range servers {
		t.Cleanup(server.Close)
	}
	user := testUser(t, a.client)
	const n = 64
	start := make(chan struct{})
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		server := servers[i%2]
		go func() {
			<-start
			res, err := server.Client().Post(server.URL+"/v1/check", "application/json", bytes.NewBufferString(fmt.Sprintf(`{"user_id":%q}`, user)))
			if err != nil {
				results <- 0
				return
			}
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			results <- res.StatusCode
		}()
	}
	close(start)
	counts := make(map[int]int)
	for i := 0; i < n; i++ {
		counts[<-results]++
	}
	if counts[200] != 5 || counts[429] != n-5 {
		t.Fatal(counts)
	}
	snapA, err := a.get(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	snapB, err := b.get(t.Context(), user)
	if err != nil || snapA == nil || snapB == nil || *snapA != *snapB {
		t.Fatal(snapA, snapB, err)
	}
	if ok, _, err := b.check(t.Context(), testUser(t, b.client)); !ok || err != nil {
		t.Fatal(ok, err)
	}
}

// Only this test uses the TCP proxy, so it can simulate an outage while sharing Redis with other tests.
func checkDisconnect(t *testing.T, addr string) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	online := true
	connections := make(map[net.Conn]bool)
	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if !online {
				mu.Unlock()
				downstream.Close()
				continue
			}
			connections[downstream] = true
			mu.Unlock()
			go func() {
				defer func() { downstream.Close(); mu.Lock(); delete(connections, downstream); mu.Unlock() }()
				upstream, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				go func() { _, _ = io.Copy(upstream, downstream); upstream.Close() }()
				_, _ = io.Copy(downstream, upstream)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close() })
	s := testStore(t, listener.Addr().String(), 1, 0.001)
	user := testUser(t, s.client)
	payload := `{"user_id":` + strconv.Quote(user) + `}`
	handler := newAPI(s, "outage-test")
	decodeResponse[map[string]any](t, request(handler, "POST", "/v1/check", payload), 200)
	mu.Lock()
	online = false
	for conn := range connections {
		conn.Close()
	}
	mu.Unlock()
	start := time.Now()
	decodeResponse[map[string]string](t, request(handler, "POST", "/v1/check", payload), 503)
	if time.Since(start) > 2*time.Second {
		t.Fatal("storage failure exceeded the deadline")
	}
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/livez", ""), 200)
	mu.Lock()
	online = true
	mu.Unlock()
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 200)
	decodeResponse[map[string]any](t, request(handler, "POST", "/v1/check", payload), 429)
}
