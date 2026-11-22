package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyPreservesLookupAndBodyLimit(t *testing.T) {
	var upstreamMethod, upstreamURI string
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMethod, upstreamURI = r.Method, r.URL.RequestURI()
		upstreamBody, _ = io.ReadAll(r.Body)
		jsonReply(w, http.StatusOK, map[string]string{"instance": "clojure"})
	}))
	defer server.Close()

	a := app{client: server.Client(), targets: map[string]string{"clojure": server.URL}}
	handler := a.routes()
	query := func(method, path string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/targets/clojure"+path, bytes.NewReader(body))
		r.ContentLength = -1 // 分块传输的客户端同样受上限约束。
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	for _, user := range []string{".", "..", "姓名 /+?#%", strings.Repeat("x", 32000)} {
		body, _ := json.Marshal(map[string]string{"user_id": user})
		w := query(http.MethodPost, "/v1/bucket-state", body)
		if w.Code != 200 || upstreamMethod != "POST" || upstreamURI != "/v1/bucket-state" || !bytes.Equal(upstreamBody, body) {
			t.Fatalf("查桶请求发生改变，ID 长度 %d，HTTP %d", len(user), w.Code)
		}
	}
	w := query(http.MethodGet, "/v1/buckets/a%2Fb", nil)
	if w.Code != 200 || upstreamMethod != "GET" || upstreamURI != "/v1/buckets/a%2Fb" {
		t.Fatalf("旧查询路径不兼容，HTTP %d，上游 %s", w.Code, upstreamURI)
	}
	// 恰好 64 KiB 的有效 JSON（尾随空白合法）可以原样到达上游。
	body := []byte(`{"user_id":"edge"}`)
	body = append(body, bytes.Repeat([]byte(" "), (64<<10)-len(body))...)
	for _, path := range []string{"/v1/check", "/v1/bucket-state"} {
		if w := query(http.MethodPost, path, body); w.Code != 200 || !bytes.Equal(upstreamBody, body) {
			t.Fatalf("%s 错误处理了边界长度请求", path)
		}
		upstreamURI = ""
		w := query(http.MethodPost, path, append(body, ' '))
		var result map[string]string
		if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result["error"] != "invalid_request" || upstreamURI != "" {
			t.Fatalf("%s 超限应在代理返回输入错误，实际 HTTP %d，响应 %s", path, w.Code, w.Body.String())
		}
	}
}

func TestScenarioRejectsContradictoryDecision(t *testing.T) {
	// HTTP 200 搭配 allowed=false 是协议错误，场景必须在第一行停止。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		jsonReply(w, 200, map[string]any{
			"allowed": false, "instance": "clojure",
			"bucket": map[string]any{"level": 1, "capacity": 1, "leak_rate": 1},
		})
	}))
	defer server.Close()
	a := app{targets: map[string]string{"clojure": server.URL}, client: server.Client()}
	result := a.runScenario(context.Background(), "isolation", "clojure")
	if result.Passed || len(result.Rows) != 1 {
		t.Fatalf("矛盾判定未及时识别：%+v", result)
	}
}
