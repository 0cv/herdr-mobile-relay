//go:build !linux

package main

import (
	"os"
	"testing"
)

func TestStderrIsJournalDisabled(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	t.Setenv("JOURNAL_STREAM", "12:34")
	if stderrIsJournal(file) {
		t.Fatal("non-Linux descriptor detected as journal")
	}
}
