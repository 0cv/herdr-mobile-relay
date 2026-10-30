package pibridge

import (
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func TestNoticeActivityValidation(t *testing.T) {
	entry := conversation.Entry{ID: "pi:notice", Kind: "notice", Role: "assistant", Text: "Extension notification"}
	frame := ActivityFrame{Kind: "batch", Entries: []conversation.Entry{entry}}
	if err := validateActivityFrame(frame); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"page", "snapshot"} {
		page := conversation.BrowsePage{
			Entries: []conversation.Entry{entry}, SourceRevision: "revision",
			Capabilities: &conversation.SourceCapabilities{Source: "pi_events", ActiveBranch: true},
		}
		if err := validateActivityFrame(ActivityFrame{Kind: kind, Page: &page}); err != nil {
			t.Fatalf("notice %s validation failed: %v", kind, err)
		}
	}
	for _, invalid := range []conversation.Entry{
		{ID: "pi:notice", Kind: "unknown", Role: "assistant", Text: "not allowed"},
		{ID: "pi:notice", Kind: "notice", Role: "system", Text: "not allowed"},
		{ID: "pi:notice", Kind: "notice", Role: "assistant", Text: strings.Repeat("x", 65537)},
	} {
		frame.Entries = []conversation.Entry{invalid}
		if validateActivityFrame(frame) == nil {
			t.Fatalf("invalid activity accepted: %+v", invalid)
		}
	}
}
