package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func servePiActivityFixture(t *testing.T, server *Server, respond func(pibridge.ActivityFrame, string, string, int, func(pibridge.ActivityFrame))) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "hmr-pi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket := filepath.Join(root, "herdr.sock")
	herdr, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = herdr.Close() })
	instance, err := pibridge.Instance(socket)
	if err != nil {
		t.Fatal(err)
	}
	directory := pibridge.Directory(instance)
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	path := filepath.Join(directory, fmt.Sprintf("%d.activity.sock", os.Getpid()))
	bridge, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			conn, err := bridge.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer conn.Close()
				var request struct {
					pibridge.Identity
					Challenge string `json:"challenge"`
					Operation string `json:"operation"`
					Cursor    string `json:"cursor"`
					Limit     int    `json:"limit"`
				}
				if err := json.NewDecoder(conn).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				header := pibridge.ActivityFrame{Identity: request.Identity, Challenge: request.Challenge, Incarnation: "123e4567-e89b-12d3-a456-426614174000"}
				respond(header, request.Operation, request.Cursor, request.Limit, func(frame pibridge.ActivityFrame) {
					var wire bytes.Buffer
					encoder := json.NewEncoder(&wire)
					encoder.SetEscapeHTML(false)
					if err := encoder.Encode(frame); err != nil || wire.Len() >= pibridge.ActivityMaxBytes {
						t.Errorf("bridge frame bytes = %d, error = %v", wire.Len(), err)
						return
					}
					if _, err := wire.WriteTo(conn); err != nil {
						t.Error(err)
					}
				})
			})
		}
	})
	t.Cleanup(func() { _ = bridge.Close(); workers.Wait() })
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	server.cfg.SocketPath = socket
	server.herdrC = &discoveryBackend{fixtureProcesses: &fixtureProcesses{pid: os.Getpid()}}
}

func TestPiFeedPageKeepsContinuationWhenBridgeEvictsRequestedCursor(t *testing.T) {
	const cacheLimit = 64
	const pageLimit = 24
	entries := make([]conversation.Entry, pageLimit+2)
	for i := range entries {
		entries[i] = conversation.Entry{ID: fmt.Sprintf("pi:%d", i), Role: "assistant", Text: "fixture"}
		if i > 0 && i < len(entries)-1 {
			entries[i].Tools = []conversation.ToolActivity{{ID: "read", Name: "read", Output: strings.Repeat("<", 16384)}}
		}
	}
	server := testServer()
	var mu sync.Mutex
	var order []string
	var sequence int
	cursors := make(map[string]int)
	servePiActivityFixture(t, server, func(frame pibridge.ActivityFrame, operation, cursor string, limit int, send func(pibridge.ActivityFrame)) {
		mu.Lock()
		defer mu.Unlock()
		if operation != "page" {
			t.Errorf("unexpected operation %q", operation)
			return
		}
		end := len(entries)
		if cursor != "" {
			var ok bool
			end, ok = cursors[cursor]
			if !ok {
				return
			}
		}
		start := max(0, end-limit)
		page := conversation.BrowsePage{Available: true, Entries: entries[start:end], State: conversation.BrowseReady, Mode: conversation.BrowseSnapshot, SourceRevision: "fixture:1", Capabilities: &conversation.SourceCapabilities{Source: "pi_events"}}
		if start > 0 {
			if len(order) == cacheLimit {
				delete(cursors, order[0])
				order = order[1:]
			}
			sequence++
			page.HasMore, page.NextCursor = true, fmt.Sprintf("pa:%d", sequence)
			cursors[page.NextCursor] = start
			order = append(order, page.NextCursor)
		}
		frame.Kind, frame.Page = "page", &page
		send(frame)
	})
	identity, err := server.activityIdentity(t.Context(), protocol.TargetRef{PaneID: "fixture", AgentSessionID: "fixture-session"})
	if err != nil {
		t.Fatal(err)
	}
	var oldest string
	for i := range cacheLimit {
		page, err := pibridge.ActivityPage(t.Context(), identity, "", 1)
		if err != nil || page.NextCursor == "" {
			t.Fatalf("filling cursor slot %d: cursor=%q err=%v", i, page.NextCursor, err)
		}
		if i == 0 {
			oldest = page.NextCursor
		}
	}
	page, err := readPiFeedPage(t.Context(), identity, oldest, pageLimit, "subscriber")
	if err != nil {
		t.Fatal(err)
	}
	if !page.FitsReply() || !page.HasMore || page.NextCursor != "pa:65" || page.SubscriptionID != "subscriber" || len(page.Entries) != pageLimit || page.Diagnostics.OmittedPayloads == 0 {
		t.Fatalf("fallback page: entries=%d cursor=%q subscription=%q diagnostics=%+v", len(page.Entries), page.NextCursor, page.SubscriptionID, page.Diagnostics)
	}
	for i, entry := range page.Entries {
		want := entries[i+1]
		if entry.ID != want.ID || (entry.Text == "" && len(entry.Tools) == 0) || !entry.Truncated && !reflect.DeepEqual(entry, want) {
			t.Fatalf("fallback page lost activity or changed it without a notice at index %d", i)
		}
	}
	earlier, err := readPiFeedPage(t.Context(), identity, page.NextCursor, pageLimit, "subscriber")
	if err != nil || earlier.HasMore || len(earlier.Entries) != 1 || earlier.Entries[0].ID != entries[0].ID {
		t.Fatalf("fallback continuation: entries=%+v more=%v err=%v", earlier.Entries, earlier.HasMore, err)
	}
}

func TestObservedPiRepliesFitRelayEncoderAndRetainLiveDiff(t *testing.T) {
	const session = "fixture-session"
	const revision = "fixture:1"
	const budget = 2 * 1024 * 1024
	capabilities := &conversation.SourceCapabilities{Source: "pi_events", ActiveBranch: true}
	output := strings.Repeat("<", 16384)
	entries := make([]conversation.Entry, 25)
	for i := range entries {
		entries[i] = conversation.Entry{ID: fmt.Sprintf("pi:%d", i), Role: "assistant", Text: "fixture"}
		count := 1
		if i == 0 {
			count = 24
		}
		for j := range count {
			entries[i].Tools = append(entries[i].Tools, conversation.ToolActivity{ID: fmt.Sprintf("read-%d-%d", i, j), Name: "read", Output: output})
		}
	}
	edit := conversation.ToolActivity{ID: "call-edit", Name: "edit", Input: `{"path":"a.go","oldText":"old","newText":"new"}`, State: "running"}
	active := conversation.Entry{ID: "live:tools", Role: "assistant", Tools: []conversation.ToolActivity{edit}}
	provisional := []conversation.Entry{active}
	oversized := []conversation.Entry{{ID: "live:oversized", Role: "assistant", Text: "provisional output", Tools: entries[0].Tools}}
	finished := make(chan struct{})
	server := testServer()
	servePiActivityFixture(t, server, func(frame pibridge.ActivityFrame, operation, cursor string, limit int, send func(pibridge.ActivityFrame)) {
		page := conversation.BrowsePage{Available: true, Entries: []conversation.Entry{}, State: conversation.BrowseReady, Mode: conversation.BrowseSnapshot, Sequence: 1, SourceRevision: revision, SnapshotID: "snapshot", Capabilities: capabilities, LiveState: "generating"}
		frame.Page = &page
		if operation == "subscribe" {
			frame.Kind = "snapshot"
			send(frame)
			select {
			case <-t.Context().Done():
				return
			case <-finished:
			}
			completed := edit
			completed.State, completed.Output = "completed", "saved"
			frame.Kind, frame.Page, frame.SourceRevision, frame.Sequence = "batch", nil, revision, 2
			frame.Entries = []conversation.Entry{{ID: "pi:completed", Role: "assistant", Tools: []conversation.ToolActivity{completed}}}
			frame.Remove, frame.Capabilities = []string{active.ID}, capabilities
			send(frame)
			<-t.Context().Done()
			return
		}
		frame.Kind = "page"
		end := len(entries)
		if cursor == "" {
			page.Provisional = provisional
		} else if cursor == "pa:provisional" {
			page.Provisional = oversized
			send(frame)
			return
		} else {
			end, _ = strconv.Atoi(strings.TrimPrefix(cursor, "pa:"))
		}
		start := max(0, end-limit)
		page.Entries = entries[start:end]
		if start > 0 {
			page.HasMore, page.NextCursor = true, fmt.Sprintf("pa:%d", start)
		}
		send(frame)
	})
	server.state.CommitInventory([]*coordinator.AgentState{{PaneID: "fixture", TerminalID: "terminal", ServerSessionID: "primary", Agent: "pi", SessionID: session, AgentSessionID: session, Status: "working"}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("fixture")
	target := protocol.TargetRef{PaneID: agent.PaneID, TerminalID: agent.TerminalID, ServerSessionID: agent.ServerSessionID, AgentSessionID: session, Generation: agent.Generation}
	useProductionHandler(t, server)
	conn := dialHub(t, server)
	conn.SetReadLimit(transport.MaxOutboundMessageBytes)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request := func(message map[string]any, data any) {
		t.Helper()
		message["target"], message["pane_id"], message["request_id"] = target, target.PaneID, "fixture"
		raw, _ := json.Marshal(message)
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct {
			OK    bool            `json:"ok"`
			Error string          `json:"error"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil || !reply.OK || len(reply.Data) > budget {
			t.Fatalf("reply ok=%v bytes=%d error=%q decode error=%v", reply.OK, len(reply.Data), reply.Error, err)
		}
		if err := json.Unmarshal(reply.Data, data); err != nil {
			t.Fatal(err)
		}
	}
	read := func(cursor string) conversation.BrowsePage {
		t.Helper()
		var page conversation.BrowsePage
		request(map[string]any{"type": "get_conversation_history", "observe": true, "limit": 200, "cursor": cursor}, &page)
		return page
	}
	initial := read("")
	if len(initial.Entries) < 1 || len(initial.Entries) >= len(entries) || !initial.HasMore || initial.NextCursor == "" || initial.SubscriptionID == "" {
		t.Fatalf("oversized bridge page not repaged: entries=%d has more=%v cursor=%q", len(initial.Entries), initial.HasMore, initial.NextCursor)
	}
	running := initial.Provisional[0].Tools[0]
	if running.ID != edit.ID || running.State != "running" || running.Diff == nil || running.Diff.Text != "-old\n+new" {
		t.Fatalf("running edit = %+v, diff = %+v", running, running.Diff)
	}
	end := len(entries)
	page := initial
	for end > 0 {
		start := end - len(page.Entries)
		if start < 0 || start >= end || page.SubscriptionID != initial.SubscriptionID {
			t.Fatalf("pagination failed: end=%d start=%d subscription=%q", end, start, page.SubscriptionID)
		}
		for i, got := range page.Entries {
			want := entries[start+i]
			if got.ID != want.ID || (start+i > 0 && !reflect.DeepEqual(got.Tools, want.Tools)) {
				t.Fatalf("paging skipped or clipped activity: got %s, want %s", got.ID, want.ID)
			}
		}
		if start == 0 {
			if page.HasMore || page.NextCursor != "" || !page.Entries[0].Truncated || len(page.Entries[0].Tools) == 0 || page.Diagnostics.OmittedPayloads == 0 {
				t.Fatal("oversized individual entry did not remain navigable with an omission notice")
			}
			break
		}
		if !page.HasMore || page.NextCursor != fmt.Sprintf("pa:%d", start) {
			t.Fatalf("bridge continuation lost at start=%d: %q", start, page.NextCursor)
		}
		end, page = start, read(page.NextCursor)
	}
	page = read("pa:provisional")
	if len(page.Provisional) != 1 || !page.Provisional[0].Truncated || len(page.Provisional[0].Tools) == 0 || page.Diagnostics.OmittedPayloads == 0 {
		t.Fatal("oversized provisional payload did not remain readable with an omission notice")
	}

	close(finished)
	var batch conversationBatch
	request(map[string]any{"type": "get_conversation_updates", "subscription_id": initial.SubscriptionID, "source_revision": revision, "after": initial.Sequence}, &batch)
	if batch.Kind != "batch" || batch.Sequence != 2 || len(batch.Entries) != 1 || len(batch.Entries[0].Tools) != 1 || !reflect.DeepEqual(batch.Remove, []string{active.ID}) {
		t.Fatalf("batch = %+v", batch)
	}
	completed := batch.Entries[0].Tools[0]
	if completed.ID != running.ID || completed.State != "completed" || completed.Output != "saved" || completed.Diff == nil || *completed.Diff != *running.Diff {
		t.Fatalf("completed edit = %+v, diff = %+v", completed, completed.Diff)
	}
}
