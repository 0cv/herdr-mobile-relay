package conversation

import (
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func TestKimiForegroundCWDWhitespaceIsIdentity(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/ Kimi project ", "session_whitespace"
	kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Whitespace", 1, kimiSample(t, "rewrites/before.jsonl"))
	reader := NewReader(home)
	project := ProjectContext{CWD: "/wrong", ForegroundCWD: cwd}
	page, err := reader.ReadWithProject("kimi", project, id, "", 1)
	if err != nil || !page.Available || len(page.Entries) != 1 {
		t.Fatalf("reader changed lexical cwd: %+v %v", page, err)
	}
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	for _, scope := range []BrowseScope{{Provider: "kimi", CWD: "/wrong", ForegroundCWD: cwd, SessionID: id}, {Provider: "kimi", CWD: cwd, SessionID: id}} {
		page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1})
		if err != nil || !page.Available || len(page.Entries) != 1 {
			t.Fatalf("browser changed lexical cwd: %+v %v", page, err)
		}
	}
}
