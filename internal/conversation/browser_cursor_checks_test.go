package conversation

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBrowserSingleFileCursorChecks(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "first question"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "first answer"}},
		map[string]any{"type": "user", "uuid": "u2", "message": map[string]any{"content": "second question"}},
		map[string]any{"type": "assistant", "uuid": "a2", "message": map[string]any{"content": "second answer"}},
	)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	browser := newTestBrowser(t, reader, DefaultBrowserOptions())
	scope := normalizeBrowseScope(BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID})
	source, code := browser.sourceFor(scope, false)
	if code != "" {
		t.Fatal(code)
	}
	defer source.close()
	size, revision := source.end, source.revision
	digestOf := func(start, end int64) string {
		digest, err := fileRangeDigest(t.Context(), source.file, start, end)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	valid := browseCursor{
		Revision: revision, Boundary: browseOffset(size), RangeStart: "0", RangeEnd: browseOffset(size),
		RangeDigest: digestOf(0, size),
	}
	rewrite := func(t *testing.T) {
		rewritten := bytes.Replace(original, []byte("second answer"), []byte("second Answer"), 1)
		if len(rewritten) != len(original) || bytes.Equal(rewritten, original) {
			t.Fatal("fixture rewrite must change content and keep the length")
		}
		if err := os.WriteFile(path, rewritten, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name   string
		change func(cursor *browseCursor)
		before func(t *testing.T)
		code   string
	}{
		{name: "valid cursor"},
		{
			name: "start after end",
			change: func(cursor *browseCursor) {
				cursor.RangeStart, cursor.RangeEnd = browseOffset(size), browseOffset(size-1)
			},
			code: "invalid_cursor",
		},
		{
			name:   "end before start",
			change: func(cursor *browseCursor) { cursor.RangeStart, cursor.RangeEnd = "20", "10" },
			code:   "invalid_cursor",
		},
		{
			name:   "boundary past end",
			change: func(cursor *browseCursor) { cursor.Boundary = browseOffset(size + 1) },
			code:   "invalid_cursor",
		},
		{
			name:   "empty digest",
			change: func(cursor *browseCursor) { cursor.RangeDigest = "" },
			code:   "invalid_cursor",
		},
		{
			name:   "changed revision",
			change: func(cursor *browseCursor) { cursor.Revision = "another-revision" },
			code:   "source_changed",
		},
		{
			name: "source shorter than the cursor end",
			change: func(cursor *browseCursor) {
				cursor.RangeEnd, cursor.Boundary = browseOffset(size+10), browseOffset(size+10)
			},
			code: "source_changed",
		},
		{
			name:   "same length rewrite changes the range digest",
			before: rewrite,
			code:   "source_changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"recent", "prepare"} {
				cursor := valid
				cursor.Mode = mode
				cursor.Scope = browseScopeID(scope)
				cursor.ExpiresAt = time.Now().Add(time.Hour).Unix()
				if tc.change != nil {
					tc.change(&cursor)
				}
				token, err := encodeBrowseCursor(browser.key, cursor)
				if err != nil {
					t.Fatal(err)
				}
				if tc.before != nil {
					tc.before(t)
				}
				page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Cursor: token, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				if page.ReasonCode != tc.code || (tc.code == "") != (page.Error == nil) || page.Error != nil && page.Error.Code != tc.code {
					t.Fatalf("%s cursor: reason %q, error %+v; want code %q", mode, page.ReasonCode, page.Error, tc.code)
				}
				if tc.code == "" && page.State == BrowseFailed {
					t.Fatalf("%s cursor failed: %#v", mode, page)
				}
				if tc.before != nil {
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
