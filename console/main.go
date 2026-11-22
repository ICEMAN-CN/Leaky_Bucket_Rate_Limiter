package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed web/*
var assets embed.FS

type app struct {
	targets map[string]string
	client  *http.Client
	metrics monitor
	slots   chan struct{}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func jsonReply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (a *app) history(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	var ids []string
	switch target {
	case "", "all":
		ids = instanceIDs
	case "go":
		ids = instanceIDs[1:]
	case "clojure", "go-a", "go-b":
		ids = []string{target}
	default:
		jsonReply(w, 400, map[string]string{"error": "未知监控目标"})
		return
	}
	jsonReply(w, 200, a.metrics.history(ids))
}

func (a *app) proxy(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	base, ok := a.targets[target]
	if !ok {
		jsonReply(w, 400, map[string]string{"error": "未知服务目标"})
		return
	}
	path := "/v1/check"
	if r.Method == http.MethodGet {
		path = "/v1/buckets/" + url.PathEscape(r.PathValue("user"))
	}
	body := http.MaxBytesReader(w, r.Body, 65536)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+path, body)
	if err != nil {
		jsonReply(w, 400, map[string]string{"error": "请求无效"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		jsonReply(w, 503, map[string]string{"error": "目标服务无法连接", "instance": target})
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		jsonReply(w, resp.StatusCode, map[string]string{"error": "目标服务未返回 JSON", "instance": target})
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, 200, map[string]bool{"up": true}) })
	m.HandleFunc("GET /api/history", a.history)
	m.HandleFunc("POST /api/targets/{target}/v1/check", a.proxy)
	m.HandleFunc("GET /api/targets/{target}/v1/buckets/{user}", a.proxy)
	m.HandleFunc("POST /api/scenarios/{name}", a.scenarioHTTP)
	web, _ := fs.Sub(assets, "web")
	m.Handle("GET /", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == "POST" && r.Header.Get("Origin") != "" {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || !strings.EqualFold(origin.Host, r.Host) {
				jsonReply(w, 403, map[string]string{"error": "跨站操作被拒绝"})
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}

func demo() error {
	client := http.Client{Timeout: 35 * time.Second}
	for _, demoCase := range []struct{ name, target string }{
		{"recovery", "clojure"}, {"isolation", "cluster"}, {"shared", "cluster"},
	} {
		name := demoCase.name
		resp, err := client.Post(env("CONSOLE_URL", "http://127.0.0.1:8080")+"/api/scenarios/"+name+"?target="+demoCase.target, "application/json", nil)
		if err != nil {
			return err
		}
		var result scenario
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return err
		}
		fmt.Printf("%s · %s\n", name, result.Summary)
		if resp.StatusCode != 200 || !result.Passed {
			return fmt.Errorf("场景 %s 验收失败", name)
		}
	}
	return nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "demo" {
		if err := demo(); err != nil {
			log.Fatal(err)
		}
		return
	}
	a := app{
		targets: map[string]string{
			"clojure": env("CLOJURE_URL", "http://clojure:8080"),
			"cluster": env("CLUSTER_URL", "http://gateway:8080"),
			"go-a":    env("GO_A_URL", "http://go-a:8080"), "go-b": env("GO_B_URL", "http://go-b:8080"),
		},
		client: &http.Client{Timeout: 1500 * time.Millisecond}, slots: make(chan struct{}, 8),
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			a.metrics.collect(ctx, a.client, a.targets)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	s := http.Server{Addr: ":" + env("PORT", "8080"), Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = s.Shutdown(shutdown)
	}()
	log.Print("操作台监听 ", s.Addr)
	if err := s.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
