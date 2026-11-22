package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

type measurements struct {
	Instance    string    `json:"instance"`
	StartedAt   string    `json:"started_at"`
	Allowed     uint64    `json:"allowed"`
	Limited     uint64    `json:"limited"`
	Errors      uint64    `json:"errors"`
	LatencyBins [8]uint64 `json:"latency_bins"`
}

type api struct {
	store    bucketStore
	instance string
	mu       sync.Mutex
	stats    measurements
}

func newAPI(store bucketStore, instance string) http.Handler {
	a := &api{store: store, instance: instance, stats: measurements{
		Instance: instance, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", a.check)
	mux.HandleFunc("GET /v1/buckets/{user_id}", a.snapshot)
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		send(w, http.StatusOK, map[string]string{"instance": instance, "status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.ready(r.Context()); err != nil {
			a.failure(w, http.StatusServiceUnavailable, "store_unavailable")
			return
		}
		send(w, http.StatusOK, map[string]string{"instance": instance, "status": "ok"})
	})
	mux.HandleFunc("GET /internal/metrics", func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		stats := a.stats
		a.mu.Unlock()
		send(w, http.StatusOK, stats)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		a.failure(w, http.StatusNotFound, "route_not_found")
	})
	return mux
}

func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *api) failure(w http.ResponseWriter, status int, reason string) {
	send(w, status, map[string]string{"instance": a.instance, "error": reason})
}

func (a *api) check(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var payload map[string]string
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := decoder.Decode(&payload); err != nil || len(payload) != 1 || payload["user_id"] == "" || decoder.Decode(&struct{}{}) != io.EOF {
		a.failure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	allowed, b, err := a.store.check(r.Context(), payload["user_id"])
	if err != nil {
		a.record(http.StatusServiceUnavailable, time.Since(start))
		a.failure(w, http.StatusServiceUnavailable, "store_unavailable")
		return
	}
	status := http.StatusOK
	if !allowed {
		status = http.StatusTooManyRequests
	}
	a.record(status, time.Since(start))
	send(w, status, map[string]any{"allowed": allowed, "instance": a.instance, "bucket": b})
}

var latencyLimits = [...]time.Duration{
	time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
	50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond,
}

func (a *api) record(status int, duration time.Duration) {
	bin := 0
	for bin < len(latencyLimits) && duration > latencyLimits[bin] {
		bin++
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch status {
	case http.StatusOK:
		a.stats.Allowed++
	case http.StatusTooManyRequests:
		a.stats.Limited++
	default:
		a.stats.Errors++
	}
	a.stats.LatencyBins[bin]++
}

func (a *api) snapshot(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user_id")
	if user == "" {
		a.failure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	b, err := a.store.get(r.Context(), user)
	if err != nil {
		a.failure(w, http.StatusServiceUnavailable, "store_unavailable")
		return
	}
	if b == nil {
		a.failure(w, http.StatusNotFound, "bucket_not_found")
		return
	}
	send(w, http.StatusOK, map[string]any{"instance": a.instance, "bucket": b})
}
