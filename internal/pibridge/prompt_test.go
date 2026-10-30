package pibridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func promptEndpoint(t *testing.T, change func(*PromptResponse), drop bool) (string, PromptLease, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "d-pi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	lease := PromptLease{Identity: Identity{Instance: strings.Repeat("a", 64), Pane: "pane", Session: "session", PID: os.Getpid()}, Incarnation: strings.Repeat("b", 36), Token: strings.Repeat("c", 36), Connection: "connection", TargetKey: "exact-target"}
	socket := filepath.Join(dir, fmtPID(lease.PID)+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	count := &atomic.Int32{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				var request map[string]any
				if json.NewDecoder(conn).Decode(&request) != nil {
					return
				}
				count.Add(1)
				if drop {
					return
				}
				response := PromptResponse{PromptLease: lease, Challenge: request["challenge"].(string), Status: "unconfirmed", SentAt: time.Now().UnixMilli()}
				if request["operation"] == "prompt_capability" {
					response.Status, response.Capability, response.ExpiresAt = "available", PromptCapability, time.Now().Add(15*time.Second).UnixMilli()
				}
				if change != nil {
					change(&response)
				}
				_ = json.NewEncoder(conn).Encode(response)
			}()
		}
	}()
	return dir, lease, count
}

func TestPiPromptExchange(t *testing.T) {
	dir, lease, count := promptEndpoint(t, nil, false)
	result, attempted, err := promptExchange(context.Background(), dir, lease, "", "")
	if err != nil || attempted || result.Capability != PromptCapability {
		t.Fatalf("capability: %+v %v", result, err)
	}
	result, attempted, err = promptExchange(context.Background(), dir, lease, lease.Incarnation+"_req-1", "/orchestrate-answer synthetic guidance")
	if err != nil || !attempted || result.Status != "unconfirmed" || count.Load() != 2 {
		t.Fatalf("send: %+v %v", result, err)
	}
}

func TestPiPromptRejectsResponseIdentityAndCapability(t *testing.T) {
	for _, field := range []string{"instance", "pane", "session", "pid", "incarnation", "challenge", "capability", "connection", "target", "token", "expiry"} {
		t.Run(field, func(t *testing.T) {
			dir, lease, _ := promptEndpoint(t, func(r *PromptResponse) {
				switch field {
				case "instance":
					r.Instance = "wrong"
				case "pane":
					r.Pane = "wrong"
				case "session":
					r.Session = "wrong"
				case "pid":
					r.PID++
				case "incarnation":
					r.Incarnation = "wrong"
				case "challenge":
					r.Challenge = "wrong"
				case "capability":
					r.Capability = "wrong"
				case "connection":
					r.Connection = "wrong"
				case "target":
					r.TargetKey = "wrong"
				case "token":
					r.Token = "wrong"
				case "expiry":
					r.ExpiresAt = 1
				}
			}, false)
			if _, _, err := promptExchange(context.Background(), dir, lease, "", ""); err == nil {
				t.Fatal("accepted invalid capability")
			}
		})
	}
}

func TestPiPromptNoRetryAndDispatchCheck(t *testing.T) {
	dir, lease, count := promptEndpoint(t, nil, true)
	_, attempted, err := promptExchange(context.Background(), dir, lease, lease.Incarnation+"_req-1", "hello")
	if err == nil || !attempted || count.Load() != 1 {
		t.Fatalf("ambiguous write: %v %v %d", err, attempted, count.Load())
	}
	ctx := herdr.WithDispatchCheck(context.Background(), func() error { return context.Canceled })
	_, attempted, err = promptExchange(ctx, dir, lease, lease.Incarnation+"_req-2", "hello")
	if err == nil || attempted || count.Load() != 1 {
		t.Fatal("ignored admission check or replayed")
	}
}

func TestPiPromptRejectsPayloadBeforeWrite(t *testing.T) {
	for _, text := range []string{"", " ", "\x1b[A", "secret\r", strings.Repeat("x", MaxPromptBytes+1), "Attachment: ref", string([]byte{255})} {
		if ValidPromptText(text) {
			t.Fatal("accepted invalid prompt")
		}
	}
	if !ValidPromptText("界\nnormal\ttext") {
		t.Fatal("rejected ordinary prompt")
	}
	dir, lease, count := promptEndpoint(t, nil, false)
	for _, id := range []string{"bad id", strings.Repeat("x", 129), strings.Repeat("d", 36) + "_old"} {
		if _, attempted, err := promptExchange(context.Background(), dir, lease, id, "text"); err == nil || attempted {
			t.Fatal("invalid ID attempted")
		}
	}
	if count.Load() != 0 {
		t.Fatal("invalid prompt written")
	}
}
