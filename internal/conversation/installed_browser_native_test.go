//go:build herdr_sqlite_logical_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package conversation

import (
	"context"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// Logical SQL rows are planned; the real installed provider core, Browser
// cursor/budget code and owner admission execute without a native database.
func TestInstalledBrowserOpenCodeOlderHistory(t *testing.T) {
	f := newOpenCodeActualFixture(t)
	f.plan(t, true, false, "", 2)
	f.setRows(false, "", 2, openCodeFixtureRows(false))
	f.plan(t, true, false, "middle", 2)
	f.setRows(false, "middle", 2, []openCodeRow{{SessionID: "ses_test", MessageID: "old", MessageType: "user", MessageData: `{"text":"old","role":"user"}`, PartID: "p0", PartData: `{"type":"text","text":"old"}`, Total: 3, CursorFound: 1, HasMessages: 1}})
	var reader *Reader
	err := f.owner.Do(context.Background(), func(call *conversationlife.Call) error {
		hermes, err := constructInstalledHermesIn(call, "/planned/home", f.binding, f.metadata)
		if err != nil {
			return err
		}
		return constructInstalledReaderIn(call, "/planned/home", f.reader, hermes, productionInstalledReaderFileDeps(), productionInstalledOMODependencies(), &reader)
	})
	if err != nil || reader == nil {
		t.Fatalf("reader: %v", err)
	}
	browser, err := newInstalledBrowser(context.Background(), reader, f.owner, DefaultBrowserOptions(), func() ([32]byte, error) { return [32]byte{1}, nil }, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := browser.Close(); err != nil {
			t.Error(err)
		}
	})
	request := BrowseRequest{Scope: BrowseScope{Provider: "opencode", SessionID: "ses_test"}, Limit: 2}
	newest, err := browser.ReadPage(context.Background(), request)
	if err != nil || !newest.Available || newest.Mode != BrowseNative || len(newest.Entries) != 2 || newest.Entries[0].ID != "middle" || newest.Entries[1].ID != "new" || !newest.HasMore || newest.NextCursor == "" || newest.Total == nil || *newest.Total != 3 {
		t.Fatalf("newest=%+v err=%v", newest, err)
	}
	request.Cursor = newest.NextCursor
	older, err := browser.ReadPage(context.Background(), request)
	if err != nil || !older.Available || older.Mode != BrowseNative || len(older.Entries) != 1 || older.Entries[0].ID != "old" || older.HasMore || f.unplanned != 0 {
		t.Fatalf("older=%+v err=%v unplanned=%d", older, err, f.unplanned)
	}
	f.owner.Begin()
	if _, err := browser.ReadPage(context.Background(), request); !errors.Is(err, conversationlife.ErrRefused) {
		t.Fatalf("fenced browser: %v", err)
	}
}
