package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jmicoltatapias/stillfs/internal/config"
	"github.com/jmicoltatapias/stillfs/internal/logging"
)

type testHealth struct{}

func (testHealth) HealthCheck(context.Context) error { return nil }

func TestRunGracefulShutdown(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()

	cfg := config.Config{
		HTTPAddr: address, StorageRoot: t.TempDir(), ReadTimeout: time.Second, WriteTimeout: time.Second, ShutdownTimeout: time.Second,
		Database: config.DatabaseConfig{
			Host: "localhost", Port: 5432, Name: "stillfs", User: "stillfs", SSLMode: "disable",
			MaxOpenConns: 1, MaxIdleConns: 1, ConnMaxLifetime: time.Minute, ConnectTimeout: time.Second,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- run(ctx, cfg, logging.New(&bytes.Buffer{}), testHealth{}) }()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, requestErr := client.Get("http://" + address + "/health/live")
		if requestErr == nil {
			_ = response.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not start: %v", requestErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("run() returned error during shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down")
	}
}
