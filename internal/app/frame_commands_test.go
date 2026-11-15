package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

const frameApprovalPane = `Would you like to run this command?
$ make check
❯ 1. Approve
  2. Reject
Enter to select · Esc to cancel`

const frameQuestionPane = "Which deployment target?\n❯ 1. Development\n2. Staging\n3. Type something.\n4. Chat about this\nEnter to select · ↑/↓ to navigate · Esc to cancel"

func newApprovalFrameEnv(t *testing.T) (*frameEnv, map[string]any) {
	t.Helper()
	classification := question.Classify(frameApprovalPane, "codex")
	e := newFrameEnv(t, &coordinator.AgentState{
		PaneID: "pane-1", Agent: "codex", TerminalID: "terminal-1", Generation: 1, ServerSessionID: protocol.PrimaryServerSessionID,
		Status: "blocked", AttentionKind: question.AttentionApproval, Prompt: classification.Prompt, Command: classification.Command,
		Options: append([]string(nil), classification.Options...), ApprovalFingerprint: question.ApprovalFingerprint(classification),
	})
	e.backend.content = frameApprovalPane
	agent, _ := e.server.state.Agent("pane-1")
	return e, map[string]any{
		"type": "respond", "event_id": agent.BlockedEventID,
		"approval_fingerprint": agent.ApprovalFingerprint, "choice": agent.Options[0],
	}
}

func newQuestionFrameEnv(t *testing.T, status string) (*frameEnv, string) {
	t.Helper()
	interaction := question.Parse(frameQuestionPane, "claude")
	if interaction == nil {
		t.Fatal("test question did not parse")
	}
	e := newFrameEnv(t, &coordinator.AgentState{
		PaneID: "pane-1", Agent: "claude", TerminalID: "terminal-1", Generation: 1, ServerSessionID: protocol.PrimaryServerSessionID, Status: status,
	})
	e.backend.content = frameQuestionPane
	return e, interaction.ID
}

func TestFrameRespondDefaultsToTheFirstOfTwoChoices(t *testing.T) {
	first, fields := newApprovalFrameEnv(t)
	expectCommandResult(t, first.exchange(t, fields), "approval", true, "accepted", "")
	absentKeys := first.backend.called("SendKeys")
	if len(absentKeys) != 1 {
		t.Fatalf("send keys = %v, want one call", absentKeys)
	}

	explicit, fields := newApprovalFrameEnv(t)
	fields["index"], fields["total"] = 0, 2
	expectCommandResult(t, explicit.exchange(t, fields), "approval", true, "accepted", "")
	if got := explicit.backend.called("SendKeys"); !slices.Equal(got, absentKeys) {
		t.Fatalf("explicit index 0 of 2 sent %v, absent index and total sent %v", got, absentKeys)
	}

	second, fields := newApprovalFrameEnv(t)
	agent, _ := second.server.state.Agent("pane-1")
	fields["choice"] = agent.Options[1]
	expectCommandResult(t, second.exchange(t, fields), "approval", false, "failed", "Approval choices are no longer available")
	if calls := second.backend.called("SendKeys"); len(calls) != 0 {
		t.Fatalf("an absent index chose the second option: %v", calls)
	}

	chosen, fields := newApprovalFrameEnv(t)
	agent, _ = chosen.server.state.Agent("pane-1")
	fields["choice"], fields["index"], fields["total"] = agent.Options[1], 1, 2
	expectCommandResult(t, chosen.exchange(t, fields), "approval", true, "accepted", "")
	if got := chosen.backend.called("SendKeys"); len(got) != 1 || slices.Equal(got, absentKeys) {
		t.Fatalf("index 1 sent %v, want keys different from %v", got, absentKeys)
	}
}

func TestFrameRespondRefusesAnOutOfRangeChoice(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"index at the total":     {"index": 2},
		"negative index":         {"index": -1},
		"total of one":           {"total": 1},
		"total above the limit":  {"total": 21},
		"index beyond the total": {"index": 3, "total": 3},
	} {
		t.Run(name, func(t *testing.T) {
			e, fields := newApprovalFrameEnv(t)
			for key, value := range extra {
				fields[key] = value
			}
			expectCommandResult(t, e.exchange(t, fields), "approval", false, "failed", "Approval choice is no longer available")
			if calls := e.backend.called("SendKeys"); len(calls) != 0 {
				t.Fatalf("a refused choice reached Herdr: %v", calls)
			}
		})
	}
}

func TestFrameAnswerQuestionSelection(t *testing.T) {
	t.Run("a valid list is answered", func(t *testing.T) {
		e, id := newQuestionFrameEnv(t, "blocked")
		expectCommandResult(t, e.exchange(t, map[string]any{
			"type": "answer_question", "interaction_id": id, "selected_indices": []int{0},
		}), "answer_question", true, "accepted", "")
		if calls := e.backend.called("SendKeys"); len(calls) == 0 {
			t.Fatal("the answer sent no keys")
		}
	})
	for name, selection := range map[string]map[string]any{
		"absent":                {},
		"null":                  {"selected_indices": nil},
		"negative":              {"selected_indices": []int{-1}},
		"negative after a good": {"selected_indices": []int{0, -1}},
		"null element":          {"selected_indices": []any{nil}},
		"null after a good":     {"selected_indices": []any{0, nil}},
		"null before a good":    {"selected_indices": []any{nil, 2}},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			e, id := newQuestionFrameEnv(t, "blocked")
			fields := map[string]any{"type": "answer_question", "interaction_id": id}
			for key, value := range selection {
				fields[key] = value
			}
			expectCommandResult(t, e.exchange(t, fields), "answer_question", false, "failed", "invalid question selection")
			if calls := e.backend.called("Send"); len(calls) != 0 {
				t.Fatalf("a refused selection reached Herdr: %v", calls)
			}
		})
	}
	t.Run("an empty list asks for an answer", func(t *testing.T) {
		e, id := newQuestionFrameEnv(t, "blocked")
		expectCommandResult(t, e.exchange(t, map[string]any{
			"type": "answer_question", "interaction_id": id, "selected_indices": []int{},
		}), "answer_question", false, "failed", "choose an answer or enter an Other answer")
	})
	t.Run("a negative element is refused before the interaction is checked", func(t *testing.T) {
		for name, fields := range map[string]map[string]any{
			"agent no longer waiting": {"interaction_id": "anything", "selected_indices": []int{-1}},
			"stale interaction":       {"interaction_id": "stale-interaction", "selected_indices": []int{-1}},
			"no interaction":          {"selected_indices": []int{-1}},
		} {
			e, _ := newQuestionFrameEnv(t, "working")
			fields["type"] = "answer_question"
			response := e.exchange(t, fields)
			if name == "no interaction" {
				expectCommandResult(t, response, "answer_question", false, "failed", "agent and question are required")
				continue
			}
			expectCommandResult(t, response, "answer_question", false, "failed", "invalid question selection")
		}
	})
}

func TestFrameSendKeysItems(t *testing.T) {
	for name, keys := range map[string]any{
		"absent":           nil,
		"empty list":       []string{},
		"empty item":       []string{""},
		"empty item among": []string{"Enter", ""},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			e := newFrameEnv(t)
			fields := map[string]any{"type": "send_keys"}
			if keys != nil {
				fields["keys"] = keys
			}
			expectCommandResult(t, e.exchange(t, fields), "send_keys", false, "failed", "Keys and agent are required")
			if calls := e.backend.called("Send"); len(calls) != 0 {
				t.Fatalf("a refused frame reached Herdr: %v", calls)
			}
		})
	}
	t.Run("null keys are refused", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_keys", "keys": nil}), "send_keys", false, "failed", "Keys and agent are required")
	})
	t.Run("a null item is refused like an empty one", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_keys", "keys": []any{"Enter", nil}}), "send_keys", false, "failed", "Keys and agent are required")
	})
	t.Run("non-string items are invalid", func(t *testing.T) {
		e := newFrameEnv(t)
		for _, keys := range []any{[]any{1}, []any{"Enter", 2}, []any{[]string{"Enter"}}, []any{true}} {
			response := e.exchange(t, map[string]any{"type": "send_keys", "keys": keys})
			if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest {
				t.Fatalf("keys %v: error = %#v, want invalid_request", keys, apiError)
			}
		}
		if calls := e.backend.called("Send"); len(calls) != 0 {
			t.Fatalf("a refused frame reached Herdr: %v", calls)
		}
	})
	t.Run("a list is sent with its label", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_keys", "keys": []string{"Ctrl+C", "Enter"}, "activity_label": "Interrupt"}), "send_keys", true, "completed", "")
		if got := e.backend.called("SendKeys"); !slices.Equal(got, []string{"SendKeys pane-1 Ctrl+C,Enter"}) {
			t.Fatalf("send keys = %v", got)
		}
	})
}

func TestFrameSendInputNeedsTextOrKeys(t *testing.T) {
	const refused = "Valid text or keys and an agent are required"
	for name, test := range map[string]struct {
		fields map[string]any
		call   string
	}{
		"text only":        {map[string]any{"text": "hello"}, `SendInput pane-1 text="hello" keys=`},
		"keys only":        {map[string]any{"keys": []string{"Enter"}}, `SendInput pane-1 text="" keys=Enter`},
		"text and keys":    {map[string]any{"text": "hello", "keys": []string{"Enter"}}, `SendInput pane-1 text="hello" keys=Enter`},
		"text, empty keys": {map[string]any{"text": "hello", "keys": []string{}}, `SendInput pane-1 text="hello" keys=`},
		"text, null keys":  {map[string]any{"text": "hello", "keys": nil}, `SendInput pane-1 text="hello" keys=`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFrameEnv(t)
			test.fields["type"] = "send_input"
			expectCommandResult(t, e.exchange(t, test.fields), "send_input", true, "completed", "")
			if got := e.backend.called("SendInput"); !slices.Equal(got, []string{test.call}) {
				t.Fatalf("send input = %v, want %s", got, test.call)
			}
		})
	}
	for name, fields := range map[string]map[string]any{
		"neither":                 {},
		"null keys":               {"keys": nil},
		"empty keys":              {"keys": []string{}},
		"an unsupported key":      {"keys": []string{"Hyper+Enter"}},
		"an empty key":            {"keys": []string{""}},
		"keys with blank padding": {"keys": []string{" Enter"}},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			e := newFrameEnv(t)
			fields["type"] = "send_input"
			expectCommandResult(t, e.exchange(t, fields), "send_input", false, "failed", refused)
			if calls := e.backend.called("Send"); len(calls) != 0 {
				t.Fatalf("a refused frame reached Herdr: %v", calls)
			}
		})
	}
}

func TestFrameSendTerminalTextRefusesKeys(t *testing.T) {
	const refused = "Use a single line of text up to 65,536 UTF-8 bytes, without control characters; send Enter separately"
	t.Run("text alone is typed", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_terminal_text", "text": "ls -la"}), "send_terminal_text", true, "completed", "")
		if got := e.backend.called("SendInput"); !slices.Equal(got, []string{`SendInput pane-1 text="ls -la" keys=`}) {
			t.Fatalf("send input = %v", got)
		}
	})
	for name, keys := range map[string]any{"empty keys": []string{}, "keys": []string{"Enter"}} {
		t.Run(name+" are refused", func(t *testing.T) {
			e := newFrameEnv(t)
			expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_terminal_text", "text": "ls", "keys": keys}), "send_terminal_text", false, "failed", refused)
			if calls := e.backend.called("Send"); len(calls) != 0 {
				t.Fatalf("a refused frame reached Herdr: %v", calls)
			}
		})
	}
	t.Run("a control character is refused", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_terminal_text", "text": "a\nb"}), "send_terminal_text", false, "failed", refused)
	})
}

func TestFrameTabReorderInsertIndex(t *testing.T) {
	for name, test := range map[string]struct {
		fields map[string]any
		moved  string
	}{
		"first position": {map[string]any{"insert_index": 0}, "TabMove tab-1 0"},
		"a position":     {map[string]any{"insert_index": 2}, "TabMove tab-1 2"},
		"the largest":    {map[string]any{"insert_index": 10000}, "TabMove tab-1 10000"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFrameEnv(t)
			test.fields["type"] = "tab_reorder"
			expectCommandResult(t, e.exchange(t, test.fields), "tab_reorder", true, "completed", "")
			if got := e.backend.called("TabMove"); !slices.Equal(got, []string{test.moved}) {
				t.Fatalf("tab move = %v, want %s", got, test.moved)
			}
		})
	}
	for name, fields := range map[string]map[string]any{
		"absent":        {},
		"null":          {"insert_index": nil},
		"negative":      {"insert_index": -1},
		"too large":     {"insert_index": 10001},
		"way too large": {"insert_index": 1 << 40},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			e := newFrameEnv(t)
			fields["type"] = "tab_reorder"
			expectCommandResult(t, e.exchange(t, fields), "tab_reorder", false, "failed", "Tab position is invalid")
			if calls := e.backend.called("TabMove"); len(calls) != 0 {
				t.Fatalf("a refused frame reached Herdr: %v", calls)
			}
		})
	}
	for name, value := range map[string]any{"fractional": 1.5, "string": "1", "text number": "1.0"} {
		t.Run(name+" is invalid", func(t *testing.T) {
			e := newFrameEnv(t)
			response := e.exchange(t, map[string]any{"type": "tab_reorder", "insert_index": value})
			if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest {
				t.Fatalf("error = %#v, want invalid_request", apiError)
			}
			if calls := e.backend.called("TabMove"); len(calls) != 0 {
				t.Fatalf("a refused frame reached Herdr: %v", calls)
			}
		})
	}
}

func TestFrameRepeatedRequestIDReplaysTheFirstResult(t *testing.T) {
	e := newFrameEnv(t)
	frame := map[string]any{"type": "send_terminal_text", "request_id": "same", "text": "echo once"}
	expectCommandResult(t, e.exchange(t, frame), "send_terminal_text", true, "completed", "")
	expectCommandResult(t, e.exchange(t, frame), "send_terminal_text", true, "completed", "")
	if got := e.backend.called("SendInput"); len(got) != 1 {
		t.Fatalf("send input = %v, want the repeated request to be answered without a second send", got)
	}
	frame["text"] = "echo other"
	expectCommandResult(t, e.exchange(t, frame), "send_terminal_text", false, "failed", "A different response was already submitted")
	if got := e.backend.called("SendInput"); len(got) != 1 {
		t.Fatalf("send input = %v, want the conflicting request refused", got)
	}
}

func TestFrameAgentStartReadsHermesProfile(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	for _, name := range []string{"claude", "hermes"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", "")
	t.Setenv("HERDR_HERMES_DATA_DIRS", "")
	t.Setenv("PATH", bin)
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	start := func(t *testing.T, fields map[string]any) map[string]any {
		t.Helper()
		e := newFrameEnv(t)
		e.server.dispatcher.SetProfiles(e.server.profiles)
		fields["type"], fields["name"], fields["cwd"] = "agent_start", "worker", project
		fields["pane_id"] = nil
		return e.exchange(t, fields)
	}

	expectCommandResult(t, start(t, map[string]any{"profile_id": "claude", "hermes_profile": "work"}),
		"agent_start", false, "failed", "hermes_profile is only valid for Hermes agents")
	if response := start(t, map[string]any{"profile_id": "hermes", "hermes_profile": "Not Valid"}); response["ok"] != false ||
		!strings.HasPrefix(response["error"].(string), "hermes_profile must match ") {
		t.Fatalf("an invalid Hermes profile name = %#v", response)
	}
	expectCommandResult(t, start(t, map[string]any{"profile_id": "hermes", "hermes_profile": "work"}),
		"agent_start", false, "failed", `hermes profile "work" is not installed on this computer`)

	profileDir := filepath.Join(home, ".hermes", "profiles", "work")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "config.yaml"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newFrameEnv(t)
	e.server.dispatcher.SetProfiles(e.server.profiles)
	launched := e.exchange(t, map[string]any{
		"type": "agent_start", "pane_id": nil, "profile_id": "hermes", "hermes_profile": "work", "name": "worker", "cwd": project,
	})
	if launched["ok"] != false {
		t.Fatalf("launch result = %#v, want the refused launch", launched)
	}
	calls := e.backend.called("LaunchTerminal")
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "LaunchTerminal worker ") || !strings.HasSuffix(calls[0], "hermes -p work") {
		t.Fatalf("launch calls = %v, want hermes started with its profile", calls)
	}
}

func TestFrameCoordinatorFieldsMatchWithoutRegardToLetterCase(t *testing.T) {
	t.Run("Keys on send_keys", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_keys", "Keys": []string{"x"}}), "send_keys", true, "completed", "")
		if got := e.backend.called("SendKeys"); !slices.Equal(got, []string{"SendKeys pane-1 x"}) {
			t.Fatalf("send keys = %v", got)
		}
	})
	t.Run("Index on respond", func(t *testing.T) {
		e, fields := newApprovalFrameEnv(t)
		agent, _ := e.server.state.Agent("pane-1")
		fields["choice"], fields["Index"] = agent.Options[1], 1
		expectCommandResult(t, e.exchange(t, fields), "approval", true, "accepted", "")
		if got := e.backend.called("SendKeys"); len(got) != 1 {
			t.Fatalf("send keys = %v, want one call for the second option", got)
		}
	})
	t.Run("Text on send_input", func(t *testing.T) {
		e := newFrameEnv(t)
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "send_input", "Text": "hello"}), "send_input", true, "completed", "")
		if got := e.backend.called("SendInput"); !slices.Equal(got, []string{`SendInput pane-1 text="hello" keys=`}) {
			t.Fatalf("send input = %v", got)
		}
	})
}
