package main

import (
	"testing"
	"time"
)

func TestHistoryAggregatesAndShowsGaps(t *testing.T) {
	t0 := time.Unix(1000, 0)
	node := func(id, start string, n int64, bin int) reading {
		bins := make([]int64, 8)
		bins[bin] = n
		return reading{health: health{ID: id, Up: true, Ready: true}, counters: counters{Started: start, Allowed: n, Bins: bins}}
	}
	intervalAt := func(sec int, a, b reading) interval {
		return interval{at: t0.Add(time.Second * time.Duration(sec)), nodes: map[string]reading{"go-a": a, "go-b": b}}
	}
	initial := intervalAt(0, node("go-a", "a", 0, 0), node("go-b", "b", 0, 7))
	first := intervalAt(2, node("go-a", "a", 94, 0), node("go-b", "b", 6, 7))
	p := summarize(initial, first, instanceIDs[1:])
	if p.Partial || *p.Allowed != 100 || *p.RPS != 50 || *p.P95 != -1 {
		t.Fatalf("incorrect aggregation: %+v", p)
	}
	next := intervalAt(4, node("go-a", "restarted", 10, 0), node("go-b", "b", 6, 7))
	p = summarize(first, next, instanceIDs[1:])
	if !p.Partial || *p.Allowed != 0 || p.P95 != nil {
		t.Fatalf("restart was counted as new traffic: %+v", p)
	}
	p = summarize(first, next, []string{"go-a"})
	if !p.Partial || p.RPS != nil || p.Allowed != nil {
		t.Fatalf("missing data was counted as zero: %+v", p)
	}
	delayed := intervalAt(20, node("go-a", "a", 98, 0), node("go-b", "b", 10, 7))
	p = summarize(first, delayed, instanceIDs[1:])
	if !p.Partial || p.RPS != nil {
		t.Fatalf("a sampling pause should show a gap: %+v", p)
	}
}
