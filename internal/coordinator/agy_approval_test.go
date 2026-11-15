package coordinator

import (
	"context"
	"reflect"
	"strings"
	"testing"

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
			client := newFakeBackend()
			client.contents["pane-1"] = test.live
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
			d := newTestDispatcher(t, client, state)
			fingerprint, choice := approvalIdentity(t, d, "pane-1", 0)
			result := d.Handle(context.Background(), map[string]any{
				"type": "respond", "request_id": "agy-approval", "pane_id": "pane-1",
				"event_id": blockedEventID(t, d, "pane-1"), "approval_fingerprint": fingerprint,
				"choice": choice, "index": float64(0), "total": float64(len(classification.Options)),
			})
			if test.wantAccepted {
				if !result.OK || result.Phase != "accepted" {
					t.Fatalf("repaint approval result = %+v", result)
				}
				want := []fakeCall{{Method: "SendKeys", Args: []any{"pane-1", []string{"Up", "Enter"}}}}
				if got := client.callsTo("SendKeys"); !reflect.DeepEqual(got, want) {
					t.Fatalf("approval keys = %+v, want %+v", got, want)
				}
			} else {
				if result.OK || result.Error != "Approval choices are no longer available" {
					t.Fatalf("stale approval result = %+v", result)
				}
				if sends := client.callsTo("SendKeys"); len(sends) != 0 {
					t.Fatalf("stale dialog received approval keys: %+v", sends)
				}
			}
		})
	}
}
