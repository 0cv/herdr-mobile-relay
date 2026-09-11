//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestStderrIsJournal(t *testing.T) {
	first, err := os.CreateTemp(t.TempDir(), "first")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.CreateTemp(t.TempDir(), "second")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	firstStream := fileJournalStream(t, first)
	secondStream := fileJournalStream(t, second)
	firstStat := fileStat(t, first)

	for _, test := range []struct {
		name   string
		file   *os.File
		stream string
		want   bool
	}{
		{name: "matching descriptor", file: first, stream: firstStream, want: true},
		{name: "different inode", file: first, stream: secondStream, want: false},
		{name: "different device", file: first, stream: fmt.Sprintf("%d:%d", uint64(firstStat.Dev)+1, uint64(firstStat.Ino)), want: false},
		{name: "empty environment", file: first, stream: "", want: false},
		{name: "missing colon", file: first, stream: "12", want: false},
		{name: "extra colon", file: first, stream: "12:34:56", want: false},
		{name: "empty component", file: first, stream: ":34", want: false},
		{name: "nonnumeric component", file: first, stream: "12:x", want: false},
		{name: "negative component", file: first, stream: "-1:34", want: false},
		{name: "overflow", file: first, stream: "18446744073709551616:34", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("JOURNAL_STREAM", test.stream)
			if got := stderrIsJournal(test.file); got != test.want {
				t.Errorf("stderrIsJournal() = %v, want %v", got, test.want)
			}
		})
	}

	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	closedStream := fileJournalStream(t, closed)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOURNAL_STREAM", closedStream)
	if stderrIsJournal(closed) {
		t.Fatal("closed descriptor detected as journal")
	}
}

func fileJournalStream(t *testing.T, file *os.File) string {
	t.Helper()
	stat := fileStat(t, file)
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))
}

func fileStat(t *testing.T, file *os.File) *syscall.Stat_t {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("file stat type = %T, want *syscall.Stat_t", info.Sys())
	}
	return stat
}
