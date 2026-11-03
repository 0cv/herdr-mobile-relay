package conversation

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrokCachedDirectoryReplacementIsContained(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		for _, browserPath := range []bool{false, true} {
			name := "reader"
			if browserPath {
				name = "browser"
			}
			if paginated {
				name += "/paginated"
			}
			t.Run(name, func(t *testing.T) {
				reader, home := testReader(t)
				path := grokUpdatesPath(home, grokCWD, testSessionID)
				grokTurns(3).write(t, path)
				browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
				if err != nil {
					t.Fatal(err)
				}
				defer browser.Close()
				scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}
				cursor := ""
				if browserPath {
					page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
					if err != nil || len(page.Entries) != 2 || page.NextCursor == "" {
						t.Fatalf("initial page = %#v, %v", page, err)
					}
					if paginated {
						cursor = page.NextCursor
					}
				} else {
					page := readGrok(t, reader, "", 2)
					if len(page.Entries) != 2 {
						t.Fatalf("initial page = %#v", page)
					}
					if paginated {
						cursor = page.Entries[0].ID
					}
				}
				outside := t.TempDir()
				f := &grokFixture{}
				f.user(0, "OUTSIDE_SENTINEL")
				f.agent("OUTSIDE_SENTINEL")
				f.write(t, filepath.Join(outside, grokUpdatesName))
				dir := filepath.Dir(path)
				if err := os.Rename(dir, dir+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
				var payload any
				if browserPath {
					page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 2})
					if err == nil && page.Available && page.Error == nil {
						t.Fatalf("outside source accepted: %#v", page)
					}
					payload = page
				} else {
					page, err := reader.ReadFor("grok", grokCWD, testSessionID, cursor, 2)
					if err == nil && page.Available {
						t.Fatalf("outside source accepted: %#v", page)
					}
					payload = page
				}
				data, err := json.Marshal(payload)
				if err != nil || bytes.Contains(data, []byte("OUTSIDE_SENTINEL")) {
					t.Fatalf("outside content returned: %s (%v)", data, err)
				}
				if _, err := reader.ChangeToken(t.Context(), scope); err == nil {
					t.Fatal("ChangeToken accepted an outside source")
				}
			})
		}
	}
}

func TestGrokUntaggedInterjectionsRemainSeparate(t *testing.T) {
	reader, home := testReader(t)
	f := &grokFixture{}
	for _, text := range []string{"first", "second"} {
		f.add(map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": "The user sent a message while you were working:\n<user_query>" + text + "</user_query>"},
			"_meta":         map[string]any{"interjection": true},
		})
	}
	f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
	assertGrokTexts(t, readGrok(t, reader, "", 80).Entries, "user:first", "user:second")
}

func TestGrokHiddenEventsCloseUntaggedTurns(t *testing.T) {
	for _, kind := range []string{"agent_thought_chunk", "turn_completed", "hook_execution", "tool_call_update", "auto_compact_started"} {
		t.Run(kind, func(t *testing.T) {
			reader, home := testReader(t)
			f := &grokFixture{}
			f.untaggedUser("first")
			f.untaggedUser("continued")
			f.other(kind)
			f.untaggedUser("second")
			f.other(kind)
			f.rewind(1)
			f.write(t, grokUpdatesPath(home, grokCWD, testSessionID))
			page := readGrok(t, reader, "", 80)
			assertGrokTexts(t, page.Entries, "user:first\ncontinued")
			if page.SourceCorrupt {
				t.Fatal("valid rewind reported as corrupt")
			}
		})
	}
}

func TestGrokAppendPaginationAndChangeTokens(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	grokTurns(3).write(t, path)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}
	token := func() string {
		t.Helper()
		value, err := reader.ChangeToken(t.Context(), scope)
		if err != nil || value == "" {
			t.Fatalf("ChangeToken = %q, %v", value, err)
		}
		return value
	}
	initialToken := token()
	if token() != initialToken {
		t.Fatal("unchanged token is unstable")
	}
	latest, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("initial page = %#v, %v", latest, err)
	}
	appendGrokRows(t, path, func(f *grokFixture) { f.user(3, "appended"); f.agent("new answer") })
	appendedToken := token()
	if appendedToken == initialToken {
		t.Fatal("append did not change token")
	}
	older, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 2})
	if err != nil || older.Error != nil || older.SourceRevision != latest.SourceRevision {
		t.Fatalf("append broke pagination: %#v, %v", older, err)
	}
	assertGrokTexts(t, older.Entries, "user:question 1", "assistant:answer 1")
	appendGrokRows(t, path, func(f *grokFixture) { f.rewind(1) })
	if token() == appendedToken {
		t.Fatal("rewind did not change token")
	}
	stale, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: older.NextCursor, Limit: 2})
	if err != nil || stale.ReasonCode != "source_changed" || len(stale.Entries) != 0 {
		t.Fatalf("rewind accepted stale history: %#v, %v", stale, err)
	}
}

func TestGrokEqualLengthRewriteRejectsCursor(t *testing.T) {
	reader, home := testReader(t)
	path := grokUpdatesPath(home, grokCWD, testSessionID)
	grokTurns(3).write(t, path)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID}
	latest, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.NextCursor == "" {
		t.Fatalf("initial page = %#v, %v", latest, err)
	}
	before, err := reader.ChangeToken(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("question 0"), []byte("rewritten!"), 1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := reader.ChangeToken(t.Context(), scope)
	if err != nil || after == before {
		t.Fatalf("rewrite token = %q, previous %q, %v", after, before, err)
	}
	stale, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: latest.NextCursor, Limit: 2})
	if err != nil || stale.ReasonCode != "source_changed" || len(stale.Entries) != 0 {
		t.Fatalf("rewrite accepted stale cursor: %#v, %v", stale, err)
	}
}

func TestGrokRepresentativeFullFileProjection(t *testing.T) {
	f := grokTurns(2000)
	var data bytes.Buffer
	for _, row := range f.rows {
		if err := json.NewEncoder(&data).Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	projection, err := projectGrokUpdates(context.Background(), strings.NewReader(data.String()))
	if err != nil || len(projection.Entries) != 4000 {
		t.Fatalf("projection entries = %d, %v", len(projection.Entries), err)
	}
	t.Logf("synthetic full-file projection: %d bytes, %d entries, %s", data.Len(), len(projection.Entries), time.Since(start))
}
