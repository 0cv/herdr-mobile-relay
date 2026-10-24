package pibridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func activityFixture(t *testing.T, respond func(map[string]any) any) Identity {
	t.Helper()
	return activityPeerFixture(t, func(conn net.Conn) {
		var request map[string]any
		if json.NewDecoder(conn).Decode(&request) != nil {
			return
		}
		_ = json.NewEncoder(conn).Encode(respond(request))
	})
}

func activityPeerFixture(t *testing.T, handle func(net.Conn)) Identity {
	t.Helper()
	id := Identity{Instance: testActivityInstance(t), Pane: "fixture", Session: "fixture-session", PID: os.Getpid()}
	directory := Directory(id.Instance)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(directory, fmtActivitySocket(id.PID)))
	if err != nil {
		os.RemoveAll(directory)
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	workers.Add(1)
	t.Cleanup(func() {
		cancel()
		listener.Close()
		workers.Wait()
		os.RemoveAll(directory)
	})
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				handle(conn)
			}()
		}
	}()
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		t.Fatal(err)
	}
	return id
}
func fmtActivitySocket(pid int) string { return fmt.Sprintf("%d.activity.sock", pid) }
func goodActivityFrame(request map[string]any) map[string]any {
	return map[string]any{"instance": request["instance"], "pane": request["pane"], "session": request["session"], "pid": request["pid"], "challenge": request["challenge"], "incarnation": "123e4567-e89b-12d3-a456-426614174000", "kind": "page", "page": conversation.BrowsePage{Available: true, Entries: []conversation.Entry{{ID: "pi:a", Role: "assistant", Text: "fixture"}}, State: conversation.BrowseReady, Mode: conversation.BrowseSnapshot, SourceRevision: "fixture:1", SnapshotID: "snapshot", Capabilities: &conversation.SourceCapabilities{Source: "pi_events", ActiveBranch: true}}}
}
func TestActivityPagePrivatePeerAndNormalizedSchema(t *testing.T) {
	id := activityFixture(t, func(request map[string]any) any { return goodActivityFrame(request) })
	page, err := ActivityPage(t.Context(), id, "", 200)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Text != "fixture" {
		t.Fatalf("page=%+v error=%v", page, err)
	}
}
func TestActivityPagePreservesBoundedToolProgressAndTruncation(t *testing.T) {
	id := activityFixture(t, func(request map[string]any) any {
		frame := goodActivityFrame(request)
		page := frame["page"].(conversation.BrowsePage)
		page.Provisional = []conversation.Entry{{ID: "live:tools", Role: "assistant", Tools: []conversation.ToolActivity{{ID: "call/1", ParentID: "call", Name: "fixture", Input: "{}", Output: "progress", State: "running"}}}}
		frame["page"] = page
		return frame
	})
	page, err := ActivityPage(t.Context(), id, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	tool := page.Provisional[0].Tools[0]
	if tool.State != "running" || tool.ParentID != "call" || tool.Truncated || page.Provisional[0].Truncated {
		t.Fatalf("tool metadata changed: %+v", page.Provisional)
	}
}

func TestActivityStreamsDoNotBlockHistoryPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{}, 8)
	results := make(chan error, 8)
	started := 0
	t.Cleanup(func() {
		cancel()
		for range started {
			select {
			case err := <-results:
				if err == nil {
					t.Error("cancelled stream returned no error")
				}
			case <-time.After(2 * time.Second):
				t.Error("cancelled stream did not release capacity")
				return
			}
		}
		if len(activityStreamSlots) != 0 || len(activityPageSlots) != 0 {
			t.Error("activity capacity leaked")
		}
	})
	var identities []Identity
	for range 8 {
		id := activityPeerFixture(t, func(conn net.Conn) {
			var request map[string]any
			if json.NewDecoder(conn).Decode(&request) != nil {
				return
			}
			frame := goodActivityFrame(request)
			if request["operation"] == "subscribe" {
				frame["kind"] = "snapshot"
				if json.NewEncoder(conn).Encode(frame) != nil {
					return
				}
				_ = conn.SetDeadline(time.Time{})
				var data [1]byte
				_, _ = conn.Read(data[:])
				return
			}
			page := frame["page"].(conversation.BrowsePage)
			if request["cursor"] == "pa:older" {
				page.Entries = []conversation.Entry{{ID: "pi:older", Role: "user", Text: "earlier history"}}
			} else {
				page.HasMore = true
				page.NextCursor = "pa:older"
			}
			frame["page"] = page
			_ = json.NewEncoder(conn).Encode(frame)
		})
		identities = append(identities, id)
		started++
		go func() {
			results <- ActivityStream(ctx, id, func(frame ActivityFrame) error {
				ready <- struct{}{}
				return nil
			})
		}()
	}
	for range 8 {
		select {
		case <-ready:
		case err := <-results:
			started--
			t.Fatalf("stream failed before saturation: %v", err)
		case <-ctx.Done():
			t.Fatal("streams did not reach saturation")
		}
	}
	if len(activityStreamSlots) != 8 {
		t.Fatal("stream capacity was not saturated")
	}
	if err := ActivityStream(ctx, identities[0], func(ActivityFrame) error { return nil }); err == nil || err.Error() != "activity bridge busy" {
		t.Fatalf("excess stream was not refused: %v", err)
	}
	for _, id := range identities {
		page, err := ActivityPage(ctx, id, "", 1)
		if err != nil || !page.HasMore || page.NextCursor == "" {
			t.Fatalf("history page blocked by saturated streams: page=%+v err=%v", page, err)
		}
		older, err := ActivityPage(ctx, id, page.NextCursor, 1)
		if err != nil || len(older.Entries) != 1 || older.Entries[0].ID != "pi:older" {
			t.Fatalf("older page blocked by saturated streams: page=%+v err=%v", older, err)
		}
	}
}

func TestActivityPageRejectsIdentityAndOpaquePayloads(t *testing.T) {
	for _, kind := range []string{"identity", "opaque", "oversized", "role", "tool_state", "tool_parent"} {
		t.Run(kind, func(t *testing.T) {
			id := activityFixture(t, func(request map[string]any) any {
				frame := goodActivityFrame(request)
				switch kind {
				case "identity":
					frame["pane"] = "replacement"
				case "opaque":
					frame["raw_event"] = map[string]any{"details": "must not pass"}
				case "oversized":
					frame["page"].(conversation.BrowsePage).Entries[0].Text = string(make([]byte, 65537))
				case "role":
					frame["page"].(conversation.BrowsePage).Entries[0].Role = "system"
				case "tool_state":
					frame["page"].(conversation.BrowsePage).Entries[0].Tools = []conversation.ToolActivity{{Name: "fixture", State: "unknown"}}
				case "tool_parent":
					frame["page"].(conversation.BrowsePage).Entries[0].Tools = []conversation.ToolActivity{{Name: "fixture", ParentID: string(make([]byte, 257))}}
				}
				return frame
			})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := ActivityPage(ctx, id, "", 200); err == nil {
				t.Fatal("unsafe frame accepted")
			}
		})
	}
}
