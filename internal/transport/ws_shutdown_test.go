package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestHubShutdownBoundsBlockedOnConnect(t *testing.T) {
	for _, transport := range []string{TransportWebSocket, TransportGateway, TransportWebRTC} {
		for _, mode := range []string{"deadline", "cancellation", "close-all", "cooperative"} {
			t.Run(transport+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					hub := NewHub(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
					release := make(chan struct{})
					unblock := sync.OnceFunc(func() { close(release) })
					defer unblock()
					entered := make(chan *ClientConn, 1)
					callbackDone := make(chan struct{})
					disconnected := make(chan *ClientConn, 2)
					hub.SetOnConnect(func(client *ClientConn) {
						entered <- client
						if mode == "cooperative" {
							<-client.Context().Done()
						} else {
							<-release
						}
						close(callbackDone)
					})
					hub.SetOnDisconnect(func(client *ClientConn) { disconnected <- client })
					conn := newPipeConn(CodecJSON, transport, 8, func(context.Context, []byte) error { return nil }, nil)
					serveDone := make(chan struct{})
					go func() {
						hub.Serve(context.Background(), conn)
						close(serveDone)
					}()
					client := <-entered
					synctest.Wait()
					if hub.ClientCount() != 0 {
						t.Fatal("callback published a client before admission completed")
					}
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer cancel()
					result := make(chan error, 1)
					started := time.Now()
					go func() {
						if mode == "close-all" {
							hub.CloseAll()
							result <- nil
							return
						}
						result <- hub.Shutdown(ctx)
					}()
					if mode == "cancellation" {
						synctest.Wait()
						cancel()
					}
					err := <-result
					synctest.Wait()
					switch mode {
					case "deadline":
						if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 20*time.Millisecond {
							t.Fatalf("deadline shutdown = %v after %v", err, time.Since(started))
						}
					case "cancellation":
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("canceled shutdown = %v", err)
						}
					case "close-all":
						if err != nil || time.Since(started) != 5*time.Second {
							t.Fatalf("CloseAll returned after %v: %v", time.Since(started), err)
						}
					case "cooperative":
						if err != nil || time.Since(started) >= 20*time.Millisecond {
							t.Fatalf("cooperative callback did not drain before deadline: %v", err)
						}
					}
					if mode != "cooperative" {
						select {
						case <-callbackDone:
							t.Fatal("fixture released the blocked callback before shutdown returned")
						default:
						}
					}
					if client.Context().Err() == nil {
						t.Fatal("shutdown did not cancel the unpublished client")
					}
					select {
					case <-conn.closed:
					default:
						t.Fatal("shutdown left the unpublished transport open")
					}
					select {
					case <-client.done:
					default:
						t.Fatal("shutdown left the unpublished writer running")
					}
					late := newPipeConn(CodecJSON, transport, 8, func(context.Context, []byte) error { return nil }, nil)
					hub.Serve(context.Background(), late)
					select {
					case <-late.closed:
					default:
						t.Fatal("shutdown admitted a later connection")
					}
					unblock()
					<-serveDone
					if err := hub.Shutdown(context.Background()); err != nil {
						t.Fatal(err)
					}
					if hub.ClientCount() != 0 || len(entered) != 0 || len(disconnected) != 1 {
						t.Fatal("late callback completion registered a client or duplicated lifecycle callbacks")
					}
					select {
					case <-hub.ingressDone:
					default:
						t.Fatal("final shutdown did not drain ordered ingress")
					}
				})
			})
		}
	}
}
