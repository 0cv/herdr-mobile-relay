package conversation

import (
	"database/sql"
	"testing"
)

func execTestSQL(t *testing.T, database, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("execute fixture SQL: %v", err)
	}
}
