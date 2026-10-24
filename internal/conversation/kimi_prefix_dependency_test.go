package conversation

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestKimiRecentWindowRequiresPrefixValidation(t *testing.T) {
	window := int(DefaultBrowserOptions().RecentBytes)
	header := "{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n"
	padding := strings.Repeat("{\"type\":\"turn.ended\",\"padding\":\""+strings.Repeat("x", 160)+"\"}\n", window/160+1)
	var tail strings.Builder
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&tail, "{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":\"recent %d\"}}\n", i)
	}
	journal := func(origin string) []byte {
		return []byte(header + padding + "{\"type\":\"context.append_message\",\"message\":{\"role\":\"user\",\"content\":\"OLDER_ORIGIN\",\"origin\":{\"kind\":\"" + origin + "\"}}}\n{\"type\":\"context.undo\",\"count\":1}\n" + padding + tail.String())
	}
	valid, invalid := journal("user"), journal("hook")
	if len(valid) != len(invalid) || !bytes.Equal(valid[:window], invalid[:window]) || !bytes.Equal(valid[len(valid)-window:], invalid[len(invalid)-window:]) {
		t.Fatal("counterexample must have identical bounded head and tail windows")
	}
	for _, test := range []struct {
		name string
		wire []byte
		code string
	}{
		{name: "valid-prefix", wire: valid},
		{name: "invalid-prefix", wire: invalid, code: "source_corrupt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			t.Setenv(agentroots.KimiCodeListEnv, root)
			t.Setenv("KIMI_CODE_HOME", "")
			cwd, id := "/fixture/prefix-proof", "session_prefix"
			kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Prefix proof", 1, test.wire)
			browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			var acquired int64
			browser.physicalReadObserver = func(n int64) { acquired += n }
			page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane"}, Limit: 24})
			if err != nil || page.ReasonCode != test.code || acquired != int64(len(test.wire)) {
				t.Fatalf("page=%+v err=%v bytes=%d", page, err, acquired)
			}
			if test.code == "" {
				if !page.Available || len(page.Entries) != 24 {
					t.Fatalf("valid prefix did not authorize the recent page: %+v", page)
				}
			} else if page.Available || len(page.Entries) != 0 {
				t.Fatal("unverified older origin authorized a transcript")
			}
			t.Logf("identical %d-byte head/tail windows; prefix validation: code=%q, journal bytes=%d, visible entries=%d", window, page.ReasonCode, acquired, len(page.Entries))
		})
	}
}

func TestKimiColdLimitDoesNotTrustUnverifiedTail(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/prefix-proof", "session_limit"
	wire := append([]byte("{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n"), bytes.Repeat([]byte("\n"), maxConversationBytes)...)
	wire = append(wire, []byte("{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":\"Readable but unverified tail\"}}\n")...)
	kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Bounded replay", 1, wire)
	browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	var acquired int64
	browser.physicalReadObserver = func(n int64) { acquired += n }
	page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id}, Limit: 24})
	if err != nil || page.Available || page.ReasonCode != "output_limit" || len(page.Entries) != 0 || acquired != 0 {
		t.Fatalf("replay bound bypassed: page=%+v err=%v bytes=%d", page, err, acquired)
	}
}
