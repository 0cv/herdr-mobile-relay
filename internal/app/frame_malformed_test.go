package app

import (
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func TestFrameTypedKeysRefuseTheWrongType(t *testing.T) {
	e := newFrameEnv(t)
	for _, test := range []struct{ key, good, bad any }{
		{"activity_label", "Label", 5},
		{"hermes_profile", "default", 5},
		{"language", "en", 5},
		{"speech_request_id", "speech-1", 5},
		{"content_fingerprint", "abc", 5},
		{"interval_ms", 500, "500"},
		{"enabled", true, "true"},
		{"instance", "relay", 5},
		{"sdp", "v=0", 5},
		{"candidate", "candidate:1", 5},
		{"sdp_mid", "0", 5},
		{"sdp_mline_index", 0, "0"},
		{"lines", 30, "30"},
	} {
		key := test.key.(string)
		t.Run(key, func(t *testing.T) {
			response := e.exchange(t, map[string]any{"type": "get_activity", key: test.good})
			if response["type"] != "activity_history" {
				t.Fatalf("a correct %s: response = %#v, want the activity history", key, response)
			}
			response = e.exchange(t, map[string]any{"type": "get_activity", key: test.bad})
			if response["type"] != "error" || response["request_id"] != "req-1" {
				t.Fatalf("a wrong-typed %s: response = %#v, want an error echoing the request id", key, response)
			}
			if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest {
				t.Fatalf("a wrong-typed %s: error = %#v, want invalid_request", key, apiError)
			}
		})
	}
}

func TestFrameIgnoresKeysNothingReads(t *testing.T) {
	e := newFrameEnv(t)
	for _, key := range []string{"before", "filename", "mime"} {
		for name, value := range map[string]any{
			"a string": "x", "a number": 5, "a fraction": 1.5, "a boolean": true,
			"null": nil, "an array": []any{1}, "an object": map[string]any{"a": 1},
		} {
			response := e.exchange(t, map[string]any{"type": "get_activity", key: value})
			if response["type"] != "activity_history" {
				t.Fatalf("%s as %s: response = %#v, want the activity history", key, name, response)
			}
		}
	}
}

func TestFrameSubmitPromptIgnoresTheLegacyPromptField(t *testing.T) {
	e := newFrameEnv(t)
	target := e.target(t, "pane-1")
	ref := e.uploadAttachment(t, target)
	missing := "Attachment: " + strings.Repeat("a", 32)

	expectCommandResult(t, e.exchange(t, map[string]any{
		"type": "submit_prompt", "target": target, "text": "hello", "prompt": missing,
	}), "submit_prompt", true, "completed", "")
	if prompts := e.backend.called("Prompt"); len(prompts) != 1 || prompts[0] != `Prompt pane-1 "hello"` {
		t.Fatalf("prompts = %q, want the text only", prompts)
	}

	var targetRef protocol.TargetRef
	decodeInto(t, target, &targetRef)
	in := protocol.Inbound{Text: "Attachment: " + ref, Prompt: "Attachment: " + ref, Target: &targetRef}
	expanded, err := e.server.expandPromptAttachmentReferences("submit_prompt", in)
	if err != nil {
		t.Fatal(err)
	}
	if expanded.Prompt != "Attachment: "+ref || expanded.Text == "Attachment: "+ref {
		t.Fatalf("inbound = %+v, want text expanded and prompt untouched", expanded)
	}
}

func TestAuditWorktreeRemoveListsForceOnlyWhenSet(t *testing.T) {
	for name, test := range map[string]struct {
		force   bool
		listing bool
	}{"false": {false, false}, "true": {true, true}} {
		t.Run("force "+name, func(t *testing.T) {
			e := newFrameEnv(t)
			e.exchange(t, map[string]any{"type": "worktree_remove", "workspace_id": "w1", "force": test.force})
			details := e.auditAttempt(t, "worktree_remove").Details
			force, listed := details["force"]
			if listed != test.listing || (listed && force != true) {
				t.Fatalf("details = %#v, want force listed=%v", details, test.listing)
			}
		})
	}
}

func TestAuditWorkspaceCloseListsCloseGroupOnlyWhenSet(t *testing.T) {
	for name, listed := range map[string]bool{"false": false, "true": true} {
		t.Run("close_group "+name, func(t *testing.T) {
			e := newFrameEnv(t)
			e.exchange(t, map[string]any{"type": "workspace_close", "workspace_id": "w1", "close_group": listed})
			details := e.auditAttempt(t, "workspace_close").Details
			if _, present := details["close_group"]; present != listed {
				t.Fatalf("details = %#v, want close_group listed=%v", details, listed)
			}
		})
	}
}

func TestAuditFollowsTheRoutedActionWhenTheTypeKeyDiffersInCase(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"coordinator": {"TYPE": "send_keys", "keys": []string{"Enter"}},
		"handler":     {"TYPE": "rename_device", "device_id": "device-1", "name": "Phone"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFrameEnv(t)
			e.exchange(t, fields)
			if attempt := e.auditAttempt(t, strings.ToLower(fields["TYPE"].(string))); attempt.RequestID != "req-1" {
				t.Fatalf("attempt = %#v", attempt)
			}
		})
	}
}
