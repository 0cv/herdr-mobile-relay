package conversation

import (
	"bytes"
	"context"
	"os"
	"testing"
)

type grokCaptureMutationContext struct {
	context.Context
	checks int
	mutate func()
}

func (ctx *grokCaptureMutationContext) Err() error {
	ctx.checks++
	if ctx.checks == 2 {
		ctx.mutate()
	}
	return ctx.Context.Err()
}

func TestGrokAppendDuringCapture(t *testing.T) {
	for _, paginated := range []bool{false, true} {
		for _, rewrite := range []bool{false, true} {
			name := "cursorless"
			if paginated {
				name = "paginated"
			}
			if rewrite {
				name += "/rewrite-and-append"
			}
			t.Run(name, func(t *testing.T) {
				reader, home := testReader(t)
				path := grokUpdatesPath(home, grokCWD, testSessionID)
				grokTurns(600).write(t, path)
				browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
				if err != nil {
					t.Fatal(err)
				}
				defer browser.Close()
				scope := normalizeBrowseScope(BrowseScope{Provider: "grok", CWD: grokCWD, SessionID: testSessionID})
				initial, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
				if err != nil || initial.NextCursor == "" {
					t.Fatalf("initial = %#v, %v", initial, err)
				}
				var cursor browseCursor
				if paginated {
					cursor, err = decodeBrowseCursor(browser.key, initial.NextCursor, scope)
					if err != nil {
						t.Fatal(err)
					}
				}
				mutated := false
				ctx := &grokCaptureMutationContext{Context: t.Context(), mutate: func() {
					// The second parser cancellation checkpoint follows 1,024 records.
					mutated = true
					if rewrite {
						file, err := os.OpenFile(path, os.O_RDWR, 0)
						if err != nil {
							t.Fatal(err)
						}
						data := make([]byte, 1024)
						if _, err := file.ReadAt(data, 0); err != nil {
							t.Fatal(err)
						}
						offset := bytes.Index(data, []byte("question 0"))
						if offset < 0 {
							t.Fatal("first prompt not found")
						}
						_, err = file.WriteAt([]byte("rewritten!"), int64(offset))
						file.Close()
						if err != nil {
							t.Fatal(err)
						}
					}
					appendGrokRows(t, path, func(f *grokFixture) { f.user(600, "appended"); f.agent("appended answer") })
				}}
				page, err := browser.readGrokNative(ctx, BrowseRequest{Scope: scope, Limit: 2}, cursor, cursor.NativeBefore)
				if !mutated {
					t.Fatal("capture mutation did not execute")
				}
				if rewrite {
					if err != nil || page.ReasonCode != "source_changed" || len(page.Entries) != 0 {
						t.Fatalf("rewrite accepted: %#v, %v", page, err)
					}
					return
				}
				if err != nil || !page.Available || page.Error != nil || page.SourceRevision != initial.SourceRevision {
					t.Fatalf("append invalidated capture: %#v, %v", page, err)
				}
				if paginated {
					assertGrokTexts(t, page.Entries, "user:question 598", "assistant:answer 598")
				} else {
					assertGrokTexts(t, page.Entries, "user:question 599", "assistant:answer 599")
				}
				fresh, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 2})
				if err != nil || fresh.Error != nil || fresh.SourceRevision != initial.SourceRevision {
					t.Fatalf("fresh = %#v, %v", fresh, err)
				}
				assertGrokTexts(t, fresh.Entries, "user:appended", "assistant:appended answer")
			})
		}
	}
}
