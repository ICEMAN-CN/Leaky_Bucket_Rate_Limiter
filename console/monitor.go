package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"
)

var instanceIDs = []string{"clojure", "go-a", "go-b"}
var upperMS = []int{1, 5, 10, 25, 50, 100, 500, -1}

const retention = 30 * time.Minute
const period = 2 * time.Second

type counters struct {
	Started string  `json:"started_at"`
	Allowed int64   `json:"allowed"`
	Limited int64   `json:"limited"`
	Errors  int64   `json:"errors"`
	Bins    []int64 `json:"latency_bins"`
}
type health struct {
	ID    string `json:"id"`
	Up    bool   `json:"up"`
	Ready bool   `json:"ready"`
	At    int64  `json:"at"`
}
type reading struct {
	counters
	health
}
type interval struct {
	at    time.Time
	nodes map[string]reading
}
type point struct {
	At      int64    `json:"at"`
	Allowed *int64   `json:"allowed"`
	Limited *int64   `json:"limited"`
	Errors  *int64   `json:"errors"`
	RPS     *float64 `json:"rps"`
	P95     *int     `json:"p95_ms"`
	Partial bool     `json:"partial"`
}
type monitor struct {
	mu      sync.RWMutex
	samples []interval
}

func readNode(ctx context.Context, client *http.Client, id, base string, now time.Time) reading {
	r := reading{health: health{ID: id, At: now.Unix()}}
	get := func(path string, dst any) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return false
		}
		return dst == nil || json.NewDecoder(resp.Body).Decode(dst) == nil
	}
	r.Up = get("/internal/metrics", &r.counters) && r.Started != "" && len(r.Bins) == len(upperMS)
	r.Ready = get("/readyz", nil)
	return r
}

func (m *monitor) collect(ctx context.Context, client *http.Client, targets map[string]string) {
	now := time.Now()
	ch := make(chan reading, len(instanceIDs))
	for _, id := range instanceIDs {
		go func(id string) { ch <- readNode(ctx, client, id, targets[id], now) }(id)
	}
	s := interval{at: now, nodes: make(map[string]reading)}
	for range instanceIDs {
		r := <-ch
		s.nodes[r.ID] = r
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 多留一个基准点，用来计算首个可见区间的增量。
	for len(m.samples) > 1 && m.samples[1].at.Before(now.Add(-retention)) {
		m.samples = m.samples[1:]
	}
	m.samples = append(m.samples, s)
}

func summarize(prev, cur interval, ids []string) point {
	p := point{At: cur.at.Unix()}
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 || elapsed > 2.5*period.Seconds() {
		p.Partial = true
		return p
	}
	var allowed, limited, failures int64
	var bins [8]int64
	known := 0
	for _, id := range ids {
		a, b := prev.nodes[id], cur.nodes[id]
		if !a.Up || !b.Up || a.Started != b.Started || b.Allowed < a.Allowed || b.Limited < a.Limited || b.Errors < a.Errors {
			continue
		}
		valid := true
		for i := range bins {
			if b.Bins[i] < a.Bins[i] {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		known++
		allowed += b.Allowed - a.Allowed
		limited += b.Limited - a.Limited
		failures += b.Errors - a.Errors
		for i := range bins {
			bins[i] += b.Bins[i] - a.Bins[i]
		}
	}
	p.Partial = known != len(ids)
	if known == 0 {
		return p
	}
	rps := float64(allowed+limited+failures) / elapsed
	p.Allowed, p.Limited, p.Errors, p.RPS = &allowed, &limited, &failures, &rps
	var total, cumulative int64
	for _, v := range bins {
		total += v
	}
	if total > 0 {
		rank := int64(math.Ceil(float64(total) * 0.95))
		for i, v := range bins {
			cumulative += v
			if cumulative >= rank {
				upper := upperMS[i]
				p.P95 = &upper
				break
			}
		}
	}
	return p
}

func (m *monitor) history(ids []string) map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	points := make([]point, 0, len(m.samples))
	for i := 1; i < len(m.samples); i++ {
		points = append(points, summarize(m.samples[i-1], m.samples[i], ids))
	}
	states := make([]health, 0, len(ids))
	for _, id := range ids {
		h := health{ID: id}
		if len(m.samples) > 0 {
			h = m.samples[len(m.samples)-1].nodes[id].health
		}
		states = append(states, h)
	}
	return map[string]any{"points": points, "instances": states, "window_seconds": int(retention.Seconds()), "interval_seconds": int(period.Seconds())}
}
