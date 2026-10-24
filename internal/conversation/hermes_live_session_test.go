package conversation

import (
	"fmt"
	"strings"
	"testing"
)

func TestHermesLiveOlderSessionIgnoresOtherSessionRows(t *testing.T) {
	db, browser, initial, executor := liveDatabaseFixture(t, "hermes", 2)
	executor.active = false
	liveExec(t, db, `CREATE INDEX messages_session_id ON messages(session_id); INSERT INTO sessions VALUES('other','/other','Other'); WITH RECURSIVE n(i) AS (VALUES(3) UNION ALL SELECT i+1 FROM n WHERE i<33004) INSERT INTO messages(id,session_id,role,content,timestamp,active,compacted) SELECT i,'other','assistant','unrelated',i,1,0 FROM n`)
	watch := browser.NewDatabaseWatch(initial.scope)
	page, reset := watch.Refresh(t.Context())
	if reset || !page.Available || len(page.Entries) != 2 {
		t.Fatalf("initial reset=%v page=%+v", reset, page)
	}
	executor.active = true
	for i := 0; i < 3; i++ {
		text := fmt.Sprintf("target edit %d", i)
		liveExec(t, db, `UPDATE messages SET content=? WHERE id=2`, text)
		page, reset = watch.Refresh(t.Context())
		if reset || len(page.Entries) != 1 || page.Entries[0].Text != text {
			t.Fatalf("target must stay incremental: reset=%v page=%+v", reset, page)
		}
		liveExec(t, db, `INSERT INTO messages(session_id,role,content,timestamp,active,compacted) VALUES('live-budget','assistant',?,?,1,0)`, fmt.Sprintf("target append %d", i), 40000+i)
		page, reset = watch.Refresh(t.Context())
		if reset || len(page.Entries) != 1 || !strings.Contains(page.Entries[0].Text, "target append") {
			t.Fatalf("append must stay incremental: reset=%v page=%+v", reset, page)
		}
	}
	found := false
	for plan := range executor.plans {
		found = found || strings.Contains(plan, "USING INDEX messages_session_id (session_id=? AND rowid>?)")
	}
	if !found {
		t.Fatalf("missing indexed session/rowid plan: %+v", executor.plans)
	}
}
