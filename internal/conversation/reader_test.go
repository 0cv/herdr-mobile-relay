package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const testSessionID = "123e4567-e89b-12d3-a456-426614174000"

func testReader(t *testing.T) (*Reader, string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv(agentroots.ClaudeListEnv, "")
	t.Setenv(agentroots.QoderListEnv, "")
	t.Setenv(agentroots.CodexListEnv, "")
	t.Setenv(agentroots.PiListEnv, "")
	t.Setenv(agentroots.OMPListEnv, "")
	home := t.TempDir()
	return NewReader(home), home
}

func writeRows(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeConversationFiltersInjectedAndSidechainRows(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"content": "hello"}},
		map[string]any{"type": "user", "uuid": "u2", "message": map[string]any{"content": "<system-reminder>hidden</system-reminder>"}},
		map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "answer"}, map[string]any{"type": "tool_use", "name": "Read"}}}},
		map[string]any{"type": "assistant", "uuid": "a2", "isSidechain": true, "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "subagent"}}}},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("page = %#v, want two visible turns", page)
	}
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "hello" ||
		page.Entries[1].Role != "assistant" || page.Entries[1].Text != "answer" {
		t.Fatalf("entries = %#v", page.Entries)
	}
}

func TestCodexConversationUsesResponseItemsWithoutDuplicates(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "08", "12", "rollout-2026-08-12T10-00-00-"+testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"timestamp": "2026-08-12T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "build it"}}}},
		map[string]any{"timestamp": "2026-08-12T10:00:00Z", "type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "build it"}},
		map[string]any{"timestamp": "2026-08-12T10:00:00Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "hidden instructions"}}}},
		map[string]any{"timestamp": "2026-08-12T10:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}},
	)

	page, err := reader.Read("codex", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Entries[0].Text != "build it" || page.Entries[1].Text != "done" {
		t.Fatalf("entries = %#v", page.Entries)
	}
}

func TestPiConversationConfinesReportedPath(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "session.jsonl")
	writeRows(t, path,
		map[string]any{"type": "message", "id": "u1", "timestamp": "2026-08-12T10:00:00Z", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "question"}}}},
		map[string]any{"type": "message", "id": "a1", "timestamp": "2026-08-12T10:00:01Z", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "response"}}}},
		map[string]any{"type": "message", "id": "t1", "message": map[string]any{"role": "toolResult", "content": []any{map[string]any{"type": "text", "text": "secret output"}}}},
	)
	page, err := reader.Read("pi", path, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("entries = %#v", page.Entries)
	}

	external := filepath.Join(t.TempDir(), "outside.jsonl")
	writeRows(t, external, map[string]any{"type": "message", "message": map[string]any{"role": "user", "content": "outside"}})
	outside, err := reader.Read("pi", external, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if outside.Available {
		t.Fatal("path outside the pi session root was served")
	}

	link := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "linked.jsonl")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	linked, err := reader.Read("pi", link, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Available {
		t.Fatal("symlink outside the pi session root was served")
	}
}

func TestConversationPagesOlderTurnsWithStableCursors(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "message": map[string]any{"content": "one"}},
		map[string]any{"type": "assistant", "message": map[string]any{"content": "two"}},
		map[string]any{"type": "user", "message": map[string]any{"content": "three"}},
	)
	latest, err := reader.Read("claude", testSessionID, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Entries) != 1 || latest.Entries[0].Text != "three" || !latest.HasMore {
		t.Fatalf("latest page = %#v", latest)
	}
	older, err := reader.Read("claude", testSessionID, latest.Entries[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Entries) != 2 || older.Entries[0].Text != "one" || older.Entries[1].Text != "two" || older.HasMore {
		t.Fatalf("older page = %#v", older)
	}
}

func TestClaudeConversationAssociatesToolResultsWithCallingTurn(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "assistant", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "I will inspect the file."},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"path": "README.md"}},
			}},
		},
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:01Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "file contents"},
			}},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Entries[0].Tools) != 1 {
		t.Fatalf("entries = %#v", page.Entries)
	}
	tool := page.Entries[0].Tools[0]
	if tool.ID != "tool-1" || tool.Name != "Read" ||
		!strings.Contains(tool.Input, `"path":"README.md"`) ||
		tool.Output != "file contents" || tool.Error {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestClaudeConversationKeepsArrayShapedUserPrompts(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "hi"}}},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Available || page.Total != 1 || len(page.Entries) != 1 {
		t.Fatalf("page = %#v, want one visible turn", page)
	}
	if page.Entries[0].Role != "user" || page.Entries[0].Text != "hi" {
		t.Fatalf("entries = %#v", page.Entries)
	}
}

func TestClaudeConversationDropsToolResultOnlyUserRowButKeepsToolOutput(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "assistant", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "I will inspect the file."},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"path": "README.md"}},
			}},
		},
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:01Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "file contents"},
			}},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Entries) != 1 || page.Entries[0].Role != "assistant" {
		t.Fatalf("entries = %#v, want only the calling assistant turn", page.Entries)
	}
	if len(page.Entries[0].Tools) != 1 || page.Entries[0].Tools[0].Output != "file contents" {
		t.Fatalf("tools = %#v, want the tool output attached", page.Entries[0].Tools)
	}
}

func TestClaudeConversationFiltersArrayShapedEnvelopes(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "<system-reminder>hidden</system-reminder>"},
			}},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Entries) != 0 {
		t.Fatalf("entries = %#v, want the injected envelope filtered", page.Entries)
	}
}

func TestClaudeConversationKeepsOnlyTextFromMixedUserBlocks(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "assistant", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"path": "README.md"}},
			}},
		},
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:01Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "secret file contents"},
				map[string]any{"type": "text", "text": "now summarise it"},
			}},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("entries = %#v, want the assistant call and the user prompt", page.Entries)
	}
	entry := page.Entries[1]
	if entry.Role != "user" || entry.Text != "now summarise it" {
		t.Fatalf("entry = %#v, want only the text block", entry)
	}
	if strings.Contains(entry.Text, "secret file contents") || strings.Contains(entry.Text, "tool_result") {
		t.Fatalf("entry text leaked tool_result payload: %q", entry.Text)
	}
}

func TestClaudeConversationDropsEmptyArrayUserRow(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{}},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Entries) != 0 {
		t.Fatalf("entries = %#v, want no turn for an empty content array", page.Entries)
	}
}

func TestClaudeConversationRendersArrayShapedSlashCommands(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{map[string]any{
				"type": "text",
				"text": "<command-name>/clear</command-name><command-args>x</command-args>",
			}}},
		},
		map[string]any{
			"type": "user", "timestamp": "2026-08-12T10:00:01Z",
			"message": map[string]any{"content": "<command-name>/clear</command-name><command-args>x</command-args>"},
		},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Entries) != 2 {
		t.Fatalf("entries = %#v, want both command rows", page.Entries)
	}
	if page.Entries[0].Text != "/clear x" || page.Entries[1].Text != "/clear x" {
		t.Fatalf("entries = %#v, want array and string forms rendered identically", page.Entries)
	}
}

// Regression guard for the per-block envelope filter. Before array-shaped user
// records were parsed at all they were dropped wholesale, so an injected
// <system-reminder> could not reach the phone. Parsing them made it reachable:
// humanClaudeText anchors its envelope checks on the start of the string it is
// given, so filtering the JOINED block text lets an envelope in any block after
// the first through. Filtering must happen per block.
func TestClaudeConversationDropsEnvelopeBlocksAfterTheFirst(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "please refactor the parser"},
				map[string]any{"type": "text", "text": "<system-reminder>INJECTED-SECRET</system-reminder>"},
			}}},
		map[string]any{"type": "assistant", "uuid": "a1", "timestamp": "2026-08-12T10:00:01Z",
			"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "done"}}}},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("entries = %#v, want the prompt and the answer", page.Entries)
	}
	if got := page.Entries[0].Text; got != "please refactor the parser" {
		t.Errorf("user text = %q, want the prompt block only", got)
	}
	for _, entry := range page.Entries {
		if strings.Contains(entry.Text, "INJECTED-SECRET") {
			t.Fatalf("injected system-reminder block reached the conversation view: %q", entry.Text)
		}
	}
}

// An envelope in the FIRST block must still suppress that block while later
// real prompt text survives, proving the filter is per block in both directions
// rather than an all-or-nothing check on the record.
func TestClaudeConversationKeepsPromptWhenEnvelopeComesFirst(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "timestamp": "2026-08-12T10:00:00Z",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "<system-reminder>INJECTED-SECRET</system-reminder>"},
				map[string]any{"type": "text", "text": "the actual question"},
			}}},
	)

	page, err := reader.Read("claude", testSessionID, "", 80)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Entries[0].Text != "the actual question" {
		t.Fatalf("entries = %#v, want only the real prompt text", page.Entries)
	}
}
