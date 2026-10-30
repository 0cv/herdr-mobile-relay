package pibridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

type connectionRequest struct {
	Identity
	Challenge string `json:"challenge"`
	Operation string `json:"operation"`
}

type connectionResult struct {
	mode      string
	attempted bool
	err       error
}

func connectionFixture(t *testing.T, pid int, handle func(*net.UnixConn, PromptLease) error) PromptLease {
	t.Helper()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	lease := PromptLease{
		Identity:    Identity{Instance: hex.EncodeToString(nonce[:]), Pane: "fixture", Session: "fixture-session", PID: pid},
		Incarnation: strings.Repeat("b", 36), Token: strings.Repeat("c", 36), Connection: "connection", TargetKey: "exact-target",
	}
	directory := Directory(lease.Instance)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err) // Never adopt or remove an existing directory.
	}
	t.Logf("owned connection fixture: %s", directory)
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
		t.Logf("removed connection fixture: %s", directory)
	})
	path := filepath.Join(directory, fmtPID(pid)+".sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	// Replacement tests retain the original inode without letting its listener
	// unlink a newer socket at the same pathname.
	listener.SetUnlinkOnClose(false)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		workers.Wait()
	})
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				if err := handle(conn, lease); err != nil && ctx.Err() == nil {
					t.Error(err)
				}
			}()
		}
	}()
	return lease
}

func connectionReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("connection fixture did not finish")
		var zero T
		return zero
	}
}

func connectionEOF(conn *net.UnixConn) error {
	data, err := io.ReadAll(conn)
	if err != nil || len(data) != 0 {
		return fmt.Errorf("expected zero bytes through EOF: bytes=%d error=%v", len(data), err)
	}
	return nil
}

func connectionDecode(conn *net.UnixConn, lease PromptLease) (connectionRequest, error) {
	var request connectionRequest
	err := json.NewDecoder(conn).Decode(&request)
	if err == nil && request.Identity != lease.Identity {
		err = fmt.Errorf("unexpected request identity: %+v", request.Identity)
	}
	return request, err
}

func connectionReply(conn *net.UnixConn, lease PromptLease, request connectionRequest) error {
	var response any
	switch request.Operation {
	case "catalog":
		response = Response{Identity: lease.Identity, Challenge: request.Challenge,
			Incarnation: lease.Incarnation, Revision: strings.Repeat("d", 64), Status: "available",
			Commands: []slashcmd.RuntimeCommand{{Command: slashcmd.Command{Command: "/fixture", Source: "project"},
				Metadata: slashcmd.Metadata{Kind: "extension", Provenance: &slashcmd.Provenance{Path: "/fixture.ts", Source: "local", Scope: "project", Origin: "top-level"}}}}}
	case "prompt_capability", "send_prompt_unconfirmed":
		result := PromptResponse{PromptLease: lease, Challenge: request.Challenge, Status: "unconfirmed", SentAt: time.Now().UnixMilli()}
		if request.Operation == "prompt_capability" {
			result.Status, result.Capability, result.ExpiresAt = "available", PromptCapability, time.Now().Add(15*time.Second).UnixMilli()
		}
		response = result
	default:
		return fmt.Errorf("unexpected operation: %q", request.Operation)
	}
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		return err
	}
	// The client reads to EOF; half-close first, then observe its own cleanup.
	if err := conn.CloseWrite(); err != nil {
		return err
	}
	return connectionEOF(conn)
}

func connectionCall(ctx context.Context, lease PromptLease, mode string) connectionResult {
	result := connectionResult{mode: mode}
	if mode == "catalog" {
		response, err := Query(ctx, lease.Identity)
		result.err = err
		if err == nil && (len(response.Commands) != 1 || response.Commands[0].Command.Command != "/fixture") {
			result.err = fmt.Errorf("catalog made no progress: %+v", response)
		}
		return result
	}
	requestID, text := "", ""
	if mode == "send" {
		requestID, text = lease.Incarnation+"_req", "hello"
	}
	response, attempted, err := Prompt(ctx, lease, requestID, text)
	result.attempted, result.err = attempted, err
	if err == nil && ((mode == "send" && (response.Status != "unconfirmed" || response.SentAt <= 0)) ||
		(mode == "capability" && (response.Capability != PromptCapability || response.Token != lease.Token))) {
		result.err = fmt.Errorf("prompt made no progress: %+v", response)
	}
	return result
}

func TestConnectionPublicSuccessAndFreshChallenge(t *testing.T) {
	requests := make(chan connectionRequest, 6)
	closed := make(chan error, 6)
	lease := connectionFixture(t, os.Getpid(), func(conn *net.UnixConn, lease PromptLease) error {
		request, err := connectionDecode(conn, lease)
		if err != nil {
			return err
		}
		requests <- request
		closed <- connectionReply(conn, lease, request)
		return nil
	})
	seen := make(map[string]bool)
	for range 2 {
		for _, mode := range []string{"catalog", "capability", "send"} {
			result := connectionCall(t.Context(), lease, mode)
			if result.err != nil || result.attempted != (mode == "send") {
				t.Fatalf("%s: %+v", mode, result)
			}
			request := connectionReceive(t, requests)
			decoded, err := hex.DecodeString(request.Challenge)
			if err != nil || len(decoded) != 16 || request.Challenge != strings.ToLower(request.Challenge) || seen[request.Challenge] {
				t.Fatalf("invalid or reused challenge: %q", request.Challenge)
			}
			seen[request.Challenge] = true
			if err := connectionReceive(t, closed); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestConnectionPublicWrongPeerZeroBytes(t *testing.T) {
	for _, mode := range []string{"catalog", "capability", "send"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan error, 1)
			lease := connectionFixture(t, os.Getpid()+1, func(conn *net.UnixConn, _ PromptLease) error {
				closed <- connectionEOF(conn) // Raw bytes, not merely completed JSON requests.
				return nil
			})
			result := connectionCall(t.Context(), lease, mode)
			if result.err == nil || result.attempted || !strings.Contains(result.err.Error(), "bridge") || !strings.Contains(result.err.Error(), "mismatch") {
				t.Fatalf("wrong peer: %+v", result)
			}
			if err := connectionReceive(t, closed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConnectionPublicPromptReplacement(t *testing.T) {
	for _, mode := range []string{"capability", "send"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan error, 1)
			lease := connectionFixture(t, os.Getpid(), func(conn *net.UnixConn, lease PromptLease) error {
				request, err := connectionDecode(conn, lease)
				if err != nil {
					return err
				}
				path := filepath.Join(Directory(lease.Instance), fmtPID(lease.PID)+".sock")
				if err := os.Rename(path, path+".original"); err != nil {
					return err
				}
				replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					return err
				}
				replacement.SetUnlinkOnClose(false)
				defer replacement.Close()
				if err := os.Chmod(path, 0600); err != nil {
					return err
				}
				closed <- connectionReply(conn, lease, request)
				return nil
			})
			result := connectionCall(t.Context(), lease, mode)
			if result.err == nil || result.err.Error() != "bridge replaced" || result.attempted != (mode == "send") {
				t.Fatalf("replacement: %+v", result)
			}
			if err := connectionReceive(t, closed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConnectionPublicCancellationAndDeadlines(t *testing.T) {
	for _, mode := range []string{"catalog", "capability", "send"} {
		for _, kind := range []string{"cancel", "parent", "default"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				ready, closed := make(chan connectionRequest, 1), make(chan error, 1)
				lease := connectionFixture(t, os.Getpid(), func(conn *net.UnixConn, lease PromptLease) error {
					request, err := connectionDecode(conn, lease)
					if err != nil {
						return err
					}
					ready <- request
					closed <- connectionEOF(conn)
					return nil
				})
				ctx, cancel := context.WithCancel(t.Context())
				if kind == "parent" {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 500*time.Millisecond)
				}
				results := make(chan connectionResult, 1)
				var client sync.WaitGroup
				client.Add(1)
				t.Cleanup(func() { cancel(); client.Wait() })
				start := time.Now()
				go func() {
					defer client.Done()
					results <- connectionCall(ctx, lease, mode)
				}()
				connectionReceive(t, ready) // Cancellation cannot precede receipt of the request.
				if kind == "cancel" {
					cancel()
				}
				result := connectionReceive(t, results)
				elapsed := time.Since(start)
				if result.err == nil || result.attempted != (mode == "send") || len(slots) != 0 {
					t.Fatalf("cancellation/deadline: %+v slots=%d", result, len(slots))
				}
				minimum := time.Duration(0)
				if kind == "parent" {
					minimum = 300 * time.Millisecond
					deadline, _ := ctx.Deadline()
					if time.Now().Before(deadline) {
						t.Fatal("shorter parent deadline was not reached")
					}
				}
				if kind == "default" {
					minimum = 800 * time.Millisecond
					if ctx.Err() != nil {
						t.Fatal("default timeout canceled the parent")
					}
				}
				if elapsed < minimum || elapsed > 4*time.Second {
					t.Fatalf("unexpected bounded duration: %s", elapsed)
				}
				if err := connectionReceive(t, closed); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestConnectionPublicSharedCapacity(t *testing.T) {
	ready, closed := make(chan connectionRequest, 8), make(chan error, 8)
	var hold atomic.Bool
	hold.Store(true)
	lease := connectionFixture(t, os.Getpid(), func(conn *net.UnixConn, lease PromptLease) error {
		request, err := connectionDecode(conn, lease)
		if err != nil {
			return err
		}
		if hold.Load() {
			ready <- request
			closed <- connectionEOF(conn)
			return nil
		}
		return connectionReply(conn, lease, request)
	})
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan connectionResult, 8)
	var clients sync.WaitGroup
	t.Cleanup(func() { cancel(); clients.Wait() })
	modes := []string{"catalog", "capability", "send"}
	for index := range 8 {
		mode := modes[index%len(modes)]
		clients.Add(1)
		go func() {
			defer clients.Done()
			results <- connectionCall(ctx, lease, mode)
		}()
	}
	for range 8 {
		connectionReceive(t, ready)
	}
	if len(slots) != 8 {
		t.Fatalf("shared slots were not saturated: %d", len(slots))
	}
	for _, mode := range modes {
		result := connectionCall(t.Context(), lease, mode)
		if result.err == nil || result.err.Error() != "bridge busy" || result.attempted || len(ready) != 0 || len(slots) != 8 {
			t.Fatalf("ninth operation: %+v requests=%d slots=%d", result, len(ready), len(slots))
		}
	}
	cancel()
	for range 8 {
		result := connectionReceive(t, results)
		if result.err == nil || result.attempted != (result.mode == "send") {
			t.Fatalf("held operation: %+v", result)
		}
		if err := connectionReceive(t, closed); err != nil {
			t.Fatal(err)
		}
	}
	if len(slots) != 0 {
		t.Fatalf("shared capacity leaked: %d", len(slots))
	}
	hold.Store(false)
	for _, mode := range modes {
		result := connectionCall(t.Context(), lease, mode)
		if result.err != nil || result.attempted != (mode == "send") {
			t.Fatalf("no public progress after release: %+v", result)
		}
	}
}
