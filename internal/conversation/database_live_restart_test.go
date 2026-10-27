package conversation

import (
	"os"
	"slices"
	"testing"
)

func TestDatabaseLiveDeletesMessageARefreshMissedAfterWALTruncation(t *testing.T) {
	for _, format := range []string{"v1", "v2", "hermes"} {
		for _, viewerReloads := range []bool{false, true} {
			name := format + "/watch"
			if viewerReloads {
				name = format + "/viewer"
			}
			t.Run(name, func(t *testing.T) {
				db, browser, watch, executor := liveDatabaseFixture(t, format, 3)
				executor.active = false
				liveExec(t, db, "PRAGMA wal_checkpoint(TRUNCATE)")
				if _, reset := watch.Refresh(t.Context()); !reset {
					t.Fatal("WAL truncation kept the source generation")
				}
				id, text := "m000004", "after truncation"
				if format == "hermes" {
					id = "4"
				}
				shm, err := os.OpenFile(watch.path+"-shm", os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer shm.Close()
				var unpublished, published [96]byte
				if _, err := shm.ReadAt(unpublished[:], 0); err != nil {
					t.Fatal(err)
				}
				switch format {
				case "v1":
					liveExec(t, db, `INSERT INTO message VALUES(?,'ses_livebudget',4,'{"role":"assistant"}')`, id)
					liveExec(t, db, `INSERT INTO part VALUES(?,?,?)`, "p"+id, id, `{"type":"text","text":"after truncation"}`)
				case "v2":
					liveExec(t, db, `INSERT INTO session_message VALUES(?,'ses_livebudget','assistant',4,4,4,'{"content":[{"type":"text","text":"after truncation"}]}')`, id)
				case "hermes":
					liveExec(t, db, `INSERT INTO messages(id,session_id,role,content,timestamp,active,compacted) VALUES(4,'live-budget','assistant','after truncation',4,1,0)`)
				}
				if _, err := shm.ReadAt(published[:], 0); err != nil || published == unpublished {
					t.Fatalf("write did not publish a WAL commit: %v", err)
				}
				if _, err := shm.WriteAt(unpublished[:], 0); err != nil {
					t.Fatal(err)
				}
				page, reset := watch.Refresh(t.Context())
				if !reset || len(page.Entries) != 0 {
					t.Fatalf("refresh before publication: reset=%v page=%+v", reset, page)
				}
				if _, err := shm.WriteAt(published[:], 0); err != nil {
					t.Fatal(err)
				}
				if viewerReloads {
					fresh, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: watch.scope, Limit: 200})
					if err != nil || !slices.ContainsFunc(fresh.Entries, func(entry Entry) bool { return entry.ID == id }) {
						t.Fatalf("reloaded page=%+v error=%v", fresh, err)
					}
					watch.Observe(fresh, true)
				} else {
					page, reset = watch.Refresh(t.Context())
					if reset || len(page.Entries) != 1 || page.Entries[0].ID != id || page.Entries[0].Text != text {
						t.Fatalf("published message: reset=%v page=%+v", reset, page)
					}
				}
				liveExec(t, db, "DELETE FROM "+watch.table()+" WHERE id=?", id)
				if _, reset := watch.Refresh(t.Context()); !reset {
					t.Fatal("deleting a delivered message did not reset")
				}
			})
		}
	}
}
