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
		r.ContentLength = -1 // The same limit applies to clients that send chunked bodies.
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	for _, user := range []string{".", "..", "\u59d3\u540d /+?#%", strings.Repeat("x", 32000)} {
		body, _ := json.Marshal(map[string]string{"user_id": user})
		w := query(http.MethodPost, "/v1/bucket-state", body)
		if w.Code != 200 || upstreamMethod != "POST" || upstreamURI != "/v1/bucket-state" || !bytes.Equal(upstreamBody, body) {
			t.Fatalf("bucket lookup changed, ID length %d, HTTP %d", len(user), w.Code)
		}
	}
	w := query(http.MethodGet, "/v1/buckets/a%2Fb", nil)
	if w.Code != 200 || upstreamMethod != "GET" || upstreamURI != "/v1/buckets/a%2Fb" {
		t.Fatalf("legacy lookup path failed, HTTP %d, upstream %s", w.Code, upstreamURI)
	}
	// Valid JSON at exactly 64 KiB, including trailing whitespace, can reach the upstream service unchanged.
	body := []byte(`{"user_id":"edge"}`)
	body = append(body, bytes.Repeat([]byte(" "), (64<<10)-len(body))...)
	for _, path := range []string{"/v1/check", "/v1/bucket-state"} {
		if w := query(http.MethodPost, path, body); w.Code != 200 || !bytes.Equal(upstreamBody, body) {
			t.Fatalf("%s mishandled a request at the size limit", path)
		}
		upstreamURI = ""
		w := query(http.MethodPost, path, append(body, ' '))
		var result map[string]string
		if w.Code != 400 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result["error"] != "invalid_request" || upstreamURI != "" {
			t.Fatalf("%s should reject an oversized request at the proxy, got HTTP %d, body %s", path, w.Code, w.Body.String())
		}
	}
}

func TestScenarioRejectsContradictoryDecision(t *testing.T) {
	// HTTP 200 with allowed=false violates the contract and must stop the scenario at the first row.
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
		t.Fatalf("contradictory decision did not stop the scenario: %+v", result)
	}
}
