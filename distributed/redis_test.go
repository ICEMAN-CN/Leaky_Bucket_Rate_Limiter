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
		t.Fatalf("需要本机 redis-server 或设定 REDIS_TEST_ADDR：%v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		if err := cmd.Wait(); err != nil {
			t.Logf("Redis 测试进程退出：%v, %s", err, output.String())
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
			t.Fatal("Redis 启动超时")
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
	t.Run("共享契约", func(t *testing.T) { checkVectors(t, addr) })
	t.Run("空桶回收", func(t *testing.T) { checkExpiry(t, addr) })
	t.Run("两个HTTP实例原子扣量", func(t *testing.T) { checkConcurrent(t, addr) })
	t.Run("存储网络中断并恢复", func(t *testing.T) { checkDisconnect(t, addr) })
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
			t.Errorf("速率 %g 的微秒周期为 %+v，预期 %+v", tc.rate, got, tc.want)
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
			AsOf      float64 `json:"as_of"`
		}
	}
	if err := json.Unmarshal(file, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("空契约")
	}
	// 只在测试中给纯状态转换传入时间；生产入口始终调用 Redis TIME。
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
				if (values[0].(int64) == 1) != step.Allowed || math.Abs(b.Level-step.Level) > 1e-9 || b.AsOf != step.AsOf {
					t.Fatalf("第 %d 步得到 %v；期望 %+v", i, values, step)
				}
				states[step.User] = state{values[3].(int64), values[4].(int64), int64(math.Round(b.AsOf * 1e6))}
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
	// Redis 返回绝对毫秒到期时间；不能向下取整而提前释放额度。
	expiry, err := s.client.Do(t.Context(), "PEXPIRETIME", bucketKey(user)).Int64()
	if err != nil {
		t.Fatal(err)
	}
	expected := math.Ceil(float64(math.Round(b.AsOf*1e6))/1000 + b.Level/s.rate*1000)
	if expiry != int64(expected) {
		t.Fatalf("到期 %d，预期 %.0f", expiry, expected)
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
			t.Fatal("已漏空的桶未回收")
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

// TCP 代理只作用于本用例，故障测试可以安全地和其他测试共用 Redis。
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
	handler := newAPI(s, "故障测试")
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
		t.Fatal("存储失败未在期限内返回")
	}
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/livez", ""), 200)
	mu.Lock()
	online = true
	mu.Unlock()
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 200)
	decodeResponse[map[string]any](t, request(handler, "POST", "/v1/check", payload), 429)
}
