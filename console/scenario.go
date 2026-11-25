package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"time"
)

type snapshot struct {
	Level    float64 `json:"level"`
	Capacity int     `json:"capacity"`
	Rate     float64 `json:"leak_rate"`
}
type row struct {
	User      string    `json:"user_id"`
	Target    string    `json:"target"`
	Instance  string    `json:"instance"`
	SentAt    time.Time `json:"sent_at"`
	Status    int       `json:"status"`
	MS        float64   `json:"elapsed_ms"`
	Level     *float64  `json:"level"`
	Allowed   *bool     `json:"allowed"`
	Operation string    `json:"operation"`
}
type scenario struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Expected string `json:"expected"`
	Summary  string `json:"summary"`
	Rows     []row  `json:"rows"`
}

// A scenario passes only when the status code, decision and bucket level all match.
func (r row) matchesDecision(allowed bool) bool {
	status := http.StatusTooManyRequests
	if allowed {
		status = http.StatusOK
	}
	return r.Status == status && r.Allowed != nil && *r.Allowed == allowed && r.Level != nil
}

func (a *app) step(ctx context.Context, target, user string, lookup bool) (row, *snapshot) {
	body, _ := json.Marshal(map[string]string{"user_id": user})
	method, path := "POST", "/v1/check"
	if lookup {
		method, path = "GET", "/v1/buckets/"+url.PathEscape(user)
		body = nil
	}
	start := time.Now()
	out := row{User: user, Target: target, Instance: target, Operation: method, Status: 503, SentAt: start}
	req, err := http.NewRequestWithContext(ctx, method, a.targets[target]+path, bytes.NewReader(body))
	if err != nil {
		return out, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	out.MS = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		return out, nil
	}
	defer resp.Body.Close()
	out.Status = resp.StatusCode
	var result struct {
		Instance string    `json:"instance"`
		Allowed  *bool     `json:"allowed"`
		Bucket   *snapshot `json:"bucket"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return out, nil
	}
	out.Instance = result.Instance
	out.Allowed = result.Allowed
	if result.Bucket != nil {
		out.Level = &result.Bucket.Level
	}
	return out, result.Bucket
}

var expectations = map[string]string{
	"shared":    "Go A updates the bucket and Go B can read it. Alternating requests share the capacity of one bucket.",
	"isolation": "After the first user's bucket fills, the second user's first request still passes.",
	"recovery":  "The next request is rate-limited once the bucket is full. After one unit leaks, a new request passes.",
}

func (a *app) runScenario(ctx context.Context, name, target string) scenario {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return scenario{Name: name, Summary: "Could not create a demo user."}
	}
	user := "demo-" + hex.EncodeToString(id)
	result := scenario{Name: name, Expected: expectations[name], Summary: "The responses did not match expectations. Check the request log for individual decisions.", Rows: []row{}}
	add := func(t, u string, lookup bool) (row, *snapshot) {
		line, b := a.step(ctx, t, u, lookup)
		result.Rows = append(result.Rows, line)
		return line, b
	}
	if name == "shared" {
		target = "go-a"
	}
	first, b := add(target, user, false)
	if !first.matchesDecision(true) || b == nil {
		return result
	}
	if b.Capacity < 1 || b.Capacity > 50 || b.Rate <= 0 {
		result.Summary = "Scenarios require a capacity from 1 to 50 and a positive leak rate. You can still send requests manually."
		return result
	}
	if name == "shared" {
		line, other := add("go-b", user, true)
		if line.Status != 200 || line.Instance == first.Instance || other == nil || other.Level != b.Level {
			return result
		}
	}
	for i := 1; i < b.Capacity; i++ {
		next := target
		if name == "shared" && i%2 == 1 {
			next = "go-b"
		}
		line, _ := add(next, user, false)
		if !line.matchesDecision(true) {
			return result
		}
	}
	rejectTarget := target
	if name == "shared" {
		rejectTarget = "go-b"
	}
	full, _ := add(rejectTarget, user, false)
	if !full.matchesDecision(false) {
		return result
	}
	switch name {
	case "recovery":
		waitSeconds := 1 / b.Rate
		if waitSeconds > 15 {
			result.Summary = "With the current leak rate, recovery takes more than 15 seconds. Use manual requests to test it."
			return result
		}
		timer := time.NewTimer(time.Duration(math.Ceil(waitSeconds*1000)+30) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return result
		}
		line, _ := add(target, user, false)
		if !line.matchesDecision(true) {
			return result
		}
	case "isolation":
		line, _ := add(target, user+"-b", false)
		if !line.matchesDecision(true) {
			return result
		}
	}
	result.Passed = true
	result.Summary = fmt.Sprintf("Passed · %d live requests. %s", len(result.Rows), result.Expected)
	return result
}

func (a *app) scenarioHTTP(w http.ResponseWriter, r *http.Request) {
	name, target := r.PathValue("name"), r.URL.Query().Get("target")
	if target == "" {
		target = "cluster"
	}
	if _, ok := expectations[name]; !ok {
		jsonReply(w, 404, map[string]string{"error": "Unknown scenario"})
		return
	}
	if _, ok := a.targets[target]; !ok {
		jsonReply(w, 400, map[string]string{"error": "Unknown service target"})
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		jsonReply(w, 429, map[string]string{"error": "Too many scenarios are running. Try again shortly."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	jsonReply(w, 200, a.runScenario(ctx, name, target))
}
