package hc

import (
	"context"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
)

func TestEventsIdleRealCore(t *testing.T) {
	socket, _ := hctest.StartCore(t, nil)
	client := New(socket)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, stream, err := client.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Next(ctx); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		t.Fatalf("idle stream failed before the 45s read timeout: %v", err)
	case <-time.After(47 * time.Second):
		t.Log("no stream failures or reconnects across three normal 15s heartbeat intervals")
	}
	cancel()
	<-done
}
