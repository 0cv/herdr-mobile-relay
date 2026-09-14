//go:build !linux

package main

import "os"

func stderrIsJournal(_ *os.File) bool {
	return false
}
