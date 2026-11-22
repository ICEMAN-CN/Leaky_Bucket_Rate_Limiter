package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func run() error {
	capacity, err := strconv.ParseInt(env("CAPACITY", "5"), 10, 0)
	if err != nil || capacity <= 0 || capacity > 1<<53-1 {
		return fmt.Errorf("CAPACITY 必须是 Redis Lua 可以精确表示的正整数（最大 9007199254740991）")
	}
	rate, err := strconv.ParseFloat(env("LEAK_RATE", "1.0"), 64)
	if err != nil || rate <= 0 || math.IsInf(rate, 0) || math.IsNaN(rate) {
		return fmt.Errorf("LEAK_RATE 必须为有限正数")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	instance := env("INSTANCE_ID", hostname)
	store := newRedisStore(env("REDIS_ADDR", "127.0.0.1:6379"), int(capacity), rate)
	defer store.client.Close()
	server := &http.Server{
		Addr: ":" + env("PORT", "8080"), Handler: newAPI(store, instance),
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("实例 %s 监听 %s", instance, server.Addr)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
