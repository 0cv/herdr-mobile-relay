package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

const windowSession = "123e4567-e89b-12d3-a456-426614174000"

func claudeWindowServer(t *testing.T, rows []map[string]any) (*Server, *coordinator.AgentState, string) {
	t.Helper()
	isolateProviderRoots(t)
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-work-app", windowSession+".jsonl")
	writeInvariantRows(t, path, rows...)
	server := testServer()
	server.conversationM = conversation.NewReader(home)
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", Agent: "claude", Cwd: "/work/app", SessionID: windowSession, Status: "idle",
	}}, server.state.RevisionCounter())
	agent, _ := server.state.Agent("pane-1")
	return server, agent, path
}

func extractAt(server *Server, agent *coordinator.AgentState, at time.Time) string {
	entries := server.enrichActivityResponses([]activity.Entry{{
		Kind: "finished", PaneID: agent.PaneID, Agent: agent.Agent, Session: agent.SessionID,
		Timestamp: activity.MilliTimestamp(at.UnixMilli()),
	}})
	return entries[0].Extract
}

func claudeTurn(index int, base time.Time, role, text string) map[string]any {
	row := map[string]any{
		"type": role, "uuid": fmt.Sprintf("%s-%d", role, index), "timestamp": replyRFC3339(base.Add(time.Duration(index) * time.Second)),
		"message": map[string]any{"content": text},
	}
	if role == "assistant" {
		row["message"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
	}
	return row
}

func TestEnrichActivityResponsesIgnoresRepliesOlderThanTheLast200Entries(t *testing.T) {
	base := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	var rows []map[string]any
	for turn := range 250 {
		rows = append(rows,
			claudeTurn(2*turn, base, "user", fmt.Sprintf("question %d", turn)),
			claudeTurn(2*turn+1, base, "assistant", fmt.Sprintf("reply %d", turn)),
		)
	}
	server, agent, _ := claudeWindowServer(t, rows)

	if got := extractAt(server, agent, base.Add(time.Hour)); got != "reply 249" {
		t.Fatalf("activity after the transcript = %q, want the newest reply", got)
	}
	if got := extractAt(server, agent, base.Add(time.Duration(2*150+1)*time.Second)); got != "reply 150" {
		t.Fatalf("activity inside the window = %q, want reply 150", got)
	}
	if got := extractAt(server, agent, base.Add(time.Duration(2*99+1)*time.Second)); got != "" {
		t.Fatalf("activity older than the last 200 entries = %q, want no extract", got)
	}
}

func TestEnrichActivityResponsesReadsRepliesBeyondTheBrowserRecentWindow(t *testing.T) {
	base := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	rows := []map[string]any{
		claudeTurn(0, base, "user", "question"),
		claudeTurn(1, base, "assistant", "reply outside the recent window"),
	}
	filler := strings.Repeat("x", 4096)
	for index := 2; index < 152; index++ {
		rows = append(rows, claudeTurn(index, base, "user", filler))
	}
	server, agent, path := claudeWindowServer(t, rows)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	recentBytes := int64(conversation.DefaultBrowserOptions().RecentBytes)
	if info.Size() <= 2*recentBytes || info.Size() >= 16<<20 {
		t.Fatalf("transcript is %d bytes, want between twice the %d byte recent window and 16 MiB", info.Size(), recentBytes)
	}
	if got := extractAt(server, agent, base.Add(time.Hour)); got != "reply outside the recent window" {
		t.Fatalf("extract = %q, want the reply that sits before the recent window", got)
	}
}
