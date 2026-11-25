package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type stubStore struct {
	allowed bool
	fail    bool
	users   map[string]bucket
}

func (s *stubStore) ready(context.Context) error {
	if s.fail {
		return errors.New("Simulated disconnect")
	}
	return nil
}
func (s *stubStore) get(ctx context.Context, user string) (*bucket, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	b, ok := s.users[user]
	if !ok {
		return nil, nil
	}
	return &b, nil
}
func (s *stubStore) check(ctx context.Context, user string) (bool, bucket, error) {
	if err := s.ready(ctx); err != nil {
		return false, bucket{}, err
	}
	b := bucket{1, 10, 1, 0.5}
	s.users[user] = b
	return s.allowed, b, nil
}

func request(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func decodeResponse[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d, response %s", w.Code, status, w.Body)
	}
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAPIAndMetrics(t *testing.T) {
	s := &stubStore{allowed: true, users: make(map[string]bucket)}
	handler := newAPI(s, "go-a")
	data := `{"user_id":"a/b+%"}`
	for _, invalid := range []string{"", "null", "{}", `{"user_id":42}`, `{"user_id":""}`, `{"USER_ID":"x"}`, `{"user_id":"x","timestamp":5}`, data + data} {
		decodeResponse[map[string]string](t, request(handler, "POST", "/v1/check", invalid), 400)
		decodeResponse[map[string]string](t, request(handler, "POST", "/v1/bucket-state", invalid), 400)
	}
	decodeResponse[map[string]string](t, request(handler, "GET", "/v1/buckets/new", ""), 404)
	decodeResponse[map[string]string](t, request(handler, "POST", "/v1/bucket-state", data), 404)
	type decision struct {
		Allowed  bool   `json:"allowed"`
		Instance string `json:"instance"`
		Bucket   bucket `json:"bucket"`
	}
	ok := decodeResponse[decision](t, request(handler, "POST", "/v1/check", data), 200)
	if !ok.Allowed || ok.Instance != "go-a" {
		t.Fatal(ok)
	}
	got := decodeResponse[decision](t, request(handler, "GET", "/v1/buckets/"+url.PathEscape("a/b+%"), ""), 200)
	if got.Bucket != ok.Bucket {
		t.Fatal(got)
	}
	byBody := decodeResponse[decision](t, request(handler, "POST", "/v1/bucket-state", data), 200)
	if byBody != got {
		t.Fatal(byBody, got)
	}
	s.allowed = false
	rejected := decodeResponse[decision](t, request(handler, "POST", "/v1/check", data), 429)
	if rejected.Allowed {
		t.Fatal(rejected)
	}
	s.fail = true
	decodeResponse[map[string]string](t, request(handler, "POST", "/v1/check", data), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/v1/buckets/new", ""), 503)
	decodeResponse[map[string]string](t, request(handler, "POST", "/v1/bucket-state", data), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/livez", ""), 200)
	stats := decodeResponse[measurements](t, request(handler, "GET", "/internal/metrics", ""), 200)
	var count uint64
	for _, n := range stats.LatencyBins {
		count += n
	}
	if stats.Allowed != 1 || stats.Limited != 1 || stats.Errors != 1 || count != 3 || stats.StartedAt == "" {
		t.Fatal(stats)
	}
	s.fail = false
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 200)
}

func TestSnapshotWithOpaqueIDs(t *testing.T) {
	s := &stubStore{allowed: true, users: make(map[string]bucket)}
	h := newAPI(s, "go-a")
	for _, id := range []string{".", "..", "a?/# +%", strings.Repeat("x", 16<<10)} {
		data, err := json.Marshal(map[string]string{"user_id": id})
		if err != nil {
			t.Fatal(err)
		}
		type response struct {
			Bucket bucket `json:"bucket"`
		}
		checked := decodeResponse[response](t, request(h, "POST", "/v1/check", string(data)), 200)
		snapshot := decodeResponse[response](t, request(h, "POST", "/v1/bucket-state", string(data)), 200)
		if checked != snapshot {
			t.Fatal(checked, snapshot)
		}
	}
}

func TestRawUTF8UserIDs(t *testing.T) {
	s := &stubStore{allowed: true, users: make(map[string]bucket)}
	h := newAPI(s, "go-a")
	invalid := "{\"user_id\":\"\xff\"}"
	valid := `{"user_id":"�"}`
	for _, path := range []string{"/v1/check", "/v1/bucket-state"} {
		body := decodeResponse[map[string]string](t, request(h, "POST", path, invalid), 400)
		if body["error"] != "invalid_request" {
			t.Fatal(body)
		}
	}
	decodeResponse[map[string]string](t, request(h, "POST", "/v1/bucket-state", valid), 404)
	decodeResponse[map[string]any](t, request(h, "POST", "/v1/check", valid), 200)
	decodeResponse[map[string]any](t, request(h, "POST", "/v1/bucket-state", valid), 200)
}

func TestHistogramBoundaries(t *testing.T) {
	a := &api{}
	for _, limit := range latencyLimits {
		a.record(200, limit)
	}
	a.record(200, 501*time.Millisecond)
	for i, n := range a.stats.LatencyBins {
		if n != 1 {
			t.Fatalf("histogram bucket %d has %d requests", i, n)
		}
	}
}
