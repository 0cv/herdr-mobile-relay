package transport

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestHubShutdownStopsOrderedIngress(t *testing.T) {
	hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hub.ingressDone:
	default:
		t.Fatal("ordered ingress goroutine remained live after shutdown")
	}
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}
