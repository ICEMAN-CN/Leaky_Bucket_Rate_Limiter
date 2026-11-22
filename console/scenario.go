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
	User      string   `json:"user_id"`
	Target    string   `json:"target"`
	Instance  string   `json:"instance"`
	Status    int      `json:"status"`
	MS        float64  `json:"elapsed_ms"`
	Level     *float64 `json:"level"`
	Allowed   *bool    `json:"allowed"`
	Operation string   `json:"operation"`
}
type scenario struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Expected string `json:"expected"`
	Summary  string `json:"summary"`
	Rows     []row  `json:"rows"`
}

func (a *app) step(ctx context.Context, target, user string, lookup bool) (row, *snapshot) {
	body, _ := json.Marshal(map[string]string{"user_id": user})
	method, path := "POST", "/v1/check"
	if lookup {
		method, path = "GET", "/v1/buckets/"+url.PathEscape(user)
		body = nil
	}
	start := time.Now()
	out := row{User: user, Target: target, Instance: target, Operation: method, Status: 503}
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
	"shared":    "同一用户在 Go A 写入、Go B 可读；交替请求合计只放行一个桶的容量。",
	"isolation": "装满第一个用户的桶后，第二个用户的首次请求仍能放行。",
	"recovery":  "装满后下一次请求被拒；等待恢复一份额度，再次请求放行。",
}

func (a *app) runScenario(ctx context.Context, name, target string) scenario {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return scenario{Name: name, Summary: "演示用户创建失败"}
	}
	user := "demo-" + hex.EncodeToString(id)
	result := scenario{Name: name, Expected: expectations[name], Summary: "实际响应与预期不一致，请查看逐条判定。", Rows: []row{}}
	add := func(t, u string, lookup bool) (row, *snapshot) {
		line, b := a.step(ctx, t, u, lookup)
		result.Rows = append(result.Rows, line)
		return line, b
	}
	if name == "shared" {
		target = "go-a"
	}
	first, b := add(target, user, false)
	if first.Status != 200 || b == nil {
		return result
	}
	if b.Capacity < 1 || b.Capacity > 50 || b.Rate <= 0 {
		result.Summary = "操作台场景支持容量 1 至 50、正漏速；仍可手动发起请求。"
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
		if line.Status != 200 {
			return result
		}
	}
	rejectTarget := target
	if name == "shared" {
		rejectTarget = "go-b"
	}
	full, _ := add(rejectTarget, user, false)
	if full.Status != 429 {
		return result
	}
	switch name {
	case "recovery":
		waitSeconds := 1 / b.Rate
		if waitSeconds > 15 {
			result.Summary = "当前漏速需要等待超过 15 秒，请使用手动请求验证恢复。"
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
		if line.Status != 200 {
			return result
		}
	case "isolation":
		line, _ := add(target, user+"-b", false)
		if line.Status != 200 {
			return result
		}
	}
	result.Passed = true
	result.Summary = fmt.Sprintf("通过 · %d 次真实调用。%s", len(result.Rows), result.Expected)
	return result
}

func (a *app) scenarioHTTP(w http.ResponseWriter, r *http.Request) {
	name, target := r.PathValue("name"), r.URL.Query().Get("target")
	if target == "" {
		target = "cluster"
	}
	if _, ok := expectations[name]; !ok {
		jsonReply(w, 404, map[string]string{"error": "未知场景"})
		return
	}
	if _, ok := a.targets[target]; !ok {
		jsonReply(w, 400, map[string]string{"error": "未知服务目标"})
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		jsonReply(w, 429, map[string]string{"error": "正在运行的演示较多，请稍后再试"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	jsonReply(w, 200, a.runScenario(ctx, name, target))
}
