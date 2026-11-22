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
		return errors.New("测试断连")
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
		t.Fatalf("状态 %d，预期 %d，响应 %s", w.Code, status, w.Body)
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
	}
	decodeResponse[map[string]string](t, request(handler, "GET", "/v1/buckets/new", ""), 404)
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
	s.allowed = false
	rejected := decodeResponse[decision](t, request(handler, "POST", "/v1/check", data), 429)
	if rejected.Allowed {
		t.Fatal(rejected)
	}
	s.fail = true
	decodeResponse[map[string]string](t, request(handler, "POST", "/v1/check", data), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/readyz", ""), 503)
	decodeResponse[map[string]string](t, request(handler, "GET", "/v1/buckets/new", ""), 503)
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

func TestHistogramBoundaries(t *testing.T) {
	a := &api{}
	for _, limit := range latencyLimits {
		a.record(200, limit)
	}
	a.record(200, 501*time.Millisecond)
	for i, n := range a.stats.LatencyBins {
		if n != 1 {
			t.Fatalf("区间 %d 有 %d 次", i, n)
		}
	}
}
