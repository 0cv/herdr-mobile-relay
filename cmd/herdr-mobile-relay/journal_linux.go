//go:build linux

package main

import (
	"os"
	"syscall"
)

func stderrIsJournal(stderr *os.File) bool {
	if stderr == nil {
		return false
	}

	device, inode, ok := parseJournalStream(os.Getenv("JOURNAL_STREAM"))
	if !ok {
		return false
	}

	info, err := stderr.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uint64(stat.Dev) == device && uint64(stat.Ino) == inode
}
