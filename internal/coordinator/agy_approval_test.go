package coordinator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/question"
)

func TestAgyApprovalRevalidation(t *testing.T) {
	const original = `Requesting permission for:
   touch /tmp/agy-approval-original.txt
Run this command?
> 1. Yes, run command
  2. No, cancel

  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command
esc to cancel
Context [░░░░░░░░] 11.1k/1M (1%) │ 5h [░░░░░░░░] 0% (4h59m)`
	moved := strings.Replace(original, "> 1. Yes, run command", "  1. Yes, run command", 1)
	moved = strings.Replace(moved, "  2. No, cancel", "\x1b[36m> 2. No, cancel\x1b[0m", 1)
	moved = strings.Replace(moved, "(4h59m)", "(4h58m)", 1)

	for _, test := range []struct {
		name         string
		live         string
		wantAccepted bool
	}{
		{"repaint retains approval and uses live focus", moved, true},
		{"changed command rejects stale approval", strings.ReplaceAll(moved, "agy-approval-original.txt", "agy-approval-different.txt"), false},
		{"changed prompt rejects stale approval", strings.Replace(moved, "Run this command?", "Run this command with elevated permissions?", 1), false},
		{"changed choice rejects stale approval", strings.Replace(moved, "Yes, run command", "Yes, run command once", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			panePath := filepath.Join(dir, "pane.ansi")
			if err := os.WriteFile(panePath, []byte(test.live), 0o600); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(dir, "invocations.log")
			bin := writeScript(t, dir, "herdr", "#!/bin/sh\n"+
				"printf '%s\\n' \"$*\" >> \""+record+"\"\n"+
				"if [ \"$1 $2\" = \"pane read\" ]; then\n"+
				"  cat \""+panePath+"\"\n"+
				"else\n"+
				"  printf '{\"ok\":true}\\n'\n"+
				"fi\n")
			classification := question.Classify(original, "agy")
			if classification.Kind != question.AttentionApproval {
				t.Fatalf("original classification = %+v", classification)
			}
			state := NewState(testLogger())
			state.CommitInventory([]*AgentState{{
				PaneID: "pane-1", Agent: "agy", Status: "blocked",
				AttentionKind:       question.AttentionApproval,
				Options:             append([]string(nil), classification.Options...),
				ApprovalFingerprint: question.ApprovalFingerprint(classification),
			}}, state.RevisionCounter())
			d := NewDispatcher(herdr.NewClient(bin, filepath.Join(dir, "sock")), state, nil, testLogger())
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := d.Close(ctx); err != nil {
					t.Fatalf("close dispatcher: %v", err)
				}
			})
			fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)
			result := d.Handle(context.Background(), map[string]any{
				"action": "respond", "request_id": "agy-approval", "pane_id": "pane-1",
				"event_id": blockedEventID(t, d, "pane-1"), "approval_fingerprint": fingerprint,
				"choice": choice, "index": float64(0), "total": float64(len(classification.Options)),
			})
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantAccepted {
				if !result.OK || result.Phase != "accepted" {
					t.Fatalf("repaint approval result = %+v", result)
				}
				if !strings.Contains(string(data), "pane send-keys pane-1 Up Enter") {
					t.Fatalf("approval did not navigate from reparsed live focus:\n%s", data)
				}
			} else {
				if result.OK || result.Error != "Approval choices are no longer available" {
					t.Fatalf("stale approval result = %+v", result)
				}
				if strings.Contains(string(data), "send-keys") {
					t.Fatalf("stale dialog received approval keys:\n%s", data)
				}
			}
		})
	}
}
