package main

import (
	"strconv"
	"strings"
)

func parseJournalStream(raw string) (uint64, uint64, bool) {
	if raw == "" || strings.Count(raw, ":") != 1 {
		return 0, 0, false
	}
	parts := strings.SplitN(raw, ":", 2)
	if !decimalUnsigned(parts[0]) || !decimalUnsigned(parts[1]) {
		return 0, 0, false
	}
	device, deviceErr := strconv.ParseUint(parts[0], 10, 64)
	inode, inodeErr := strconv.ParseUint(parts[1], 10, 64)
	if deviceErr != nil || inodeErr != nil {
		return 0, 0, false
	}
	return device, inode, true
}

func decimalUnsigned(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
