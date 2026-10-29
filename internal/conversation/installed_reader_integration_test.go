//go:build herdr_sqlite_logical_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package conversation

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// The public Reader operation uses the same locator, source and parser as the
// installed app. Logical SQL is deliberately not a native SQLite qualification.
func TestInstalledReaderSelectedPiAndFencedLegacy(t *testing.T) {
	f := newOpenCodeActualFixture(t)
	source := newSourceFixture()
	source.data = []byte("{\"type\":\"session\"}\n{\"type\":\"message\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"installed answer\"}]}}\n")
	const path = "/planned/source.jsonl"
	base := source.deps()
	deps := installedReaderFileDeps{
		source: installedSourceDeps{
			open: func(ctx context.Context, owner *conversationlife.Owner, p string, onClose func(error)) (*sourceOwnership, error) {
				if p != path {
					source.unplanned++
					return nil, conversationlife.ErrRefused
				}
				return base.open(ctx, owner, "/planned/source", onClose)
			},
			eval: func(p string) (string, error) {
				if p == "/planned" || p == path {
					return p, nil
				}
				source.unplanned++
				return "", conversationlife.ErrRefused
			},
			stat: func(p string) (os.FileInfo, error) {
				if p != path {
					source.unplanned++
					return nil, conversationlife.ErrRefused
				}
				return source.info(), nil
			},
			same: func(a, b os.FileInfo) bool { return a.Size() == b.Size() },
		},
		readDir: func(string) ([]os.DirEntry, error) { source.unplanned++; return nil, conversationlife.ErrRefused },
		roots: func(ctx context.Context, home, kind string) ([]string, error) {
			if home != "/planned/home" || kind != "pi" {
				source.unplanned++
				return nil, conversationlife.ErrRefused
			}
			return []string{"/planned"}, ctx.Err()
		},
	}
	var reader *Reader
	err := f.owner.Do(context.Background(), func(call *conversationlife.Call) error {
		hermes, e := constructInstalledHermesIn(call, "/planned/home", f.binding, f.metadata)
		if e != nil {
			return e
		}
		return constructInstalledReaderIn(call, "/planned/home", f.reader, hermes, deps, installedOMODependencies{roots: func(context.Context, string) ([]string, error) {
			source.unplanned++
			return nil, conversationlife.ErrRefused
		}}, &reader)
	})
	if err != nil || reader == nil {
		t.Fatalf("construction: %v", err)
	}
	page, err := reader.ReadWithProjectContext(context.Background(), "pi", ProjectContext{}, path, "", 10)
	if err != nil || !page.Available || len(page.Entries) != 1 || page.Entries[0].Text != "installed answer" || source.calls["close"] != 1 || source.unplanned != 0 || f.unplanned != 0 {
		t.Fatalf("page=%+v err=%v close=%d unplanned=%d sql_unplanned=%d", page, err, source.calls["close"], source.unplanned, f.unplanned)
	}
	if location, err := reader.LocateWithProjectContext(context.Background(), "pi", ProjectContext{}, path); err != nil || location.Path != path {
		t.Fatalf("location=%+v err=%v", location, err)
	}
	browser, err := newInstalledBrowser(context.Background(), reader, f.owner, DefaultBrowserOptions(), func() ([32]byte, error) { return [32]byte{1}, nil }, t.TempDir())
	if err != nil {
		t.Fatalf("browser construction: %v", err)
	}
	t.Cleanup(func() {
		if err := browser.Close(); err != nil {
			t.Error(err)
		}
	})
	browsed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "pi", SessionID: path}, Limit: 10})
	if err != nil || !browsed.Available || browsed.State != BrowseReady || len(browsed.Entries) != 1 || browsed.Entries[0].Text != "installed answer" || source.calls["close"] != 2 || source.unplanned != 0 {
		t.Fatalf("browser=%+v err=%v close=%d unplanned=%d", browsed, err, source.calls["close"], source.unplanned)
	}
	f.owner.Begin()
	if _, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: BrowseScope{Provider: "pi", SessionID: path}}); !errors.Is(err, conversationlife.ErrRefused) {
		t.Fatalf("fenced browser=%v", err)
	}
	if _, err := reader.Read("pi", path, "", 10); !errors.Is(err, conversationlife.ErrRefused) || source.calls["close"] != 2 {
		t.Fatalf("fenced read=%v close=%d", err, source.calls["close"])
	}
}
