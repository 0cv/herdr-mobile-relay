package main

import (
	"testing"
)

func TestParseJournalStream(t *testing.T) {
	for _, test := range []struct {
		name   string
		raw    string
		device uint64
		inode  uint64
		valid  bool
	}{
		{name: "matching values", raw: "12:34", device: 12, inode: 34, valid: true},
		{name: "zero values", raw: "0:0", valid: true},
		{name: "missing", raw: "", valid: false},
		{name: "missing separator", raw: "12", valid: false},
		{name: "too many separators", raw: "12:34:56", valid: false},
		{name: "missing device", raw: ":34", valid: false},
		{name: "missing inode", raw: "12:", valid: false},
		{name: "negative device", raw: "-1:34", valid: false},
		{name: "negative inode", raw: "12:-1", valid: false},
		{name: "nonnumeric device", raw: "x:34", valid: false},
		{name: "nonnumeric inode", raw: "12:x", valid: false},
		{name: "plus sign", raw: "+12:34", valid: false},
		{name: "whitespace", raw: "12: 34", valid: false},
		{name: "device overflow", raw: "18446744073709551616:34", valid: false},
		{name: "inode overflow", raw: "12:18446744073709551616", valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			device, inode, valid := parseJournalStream(test.raw)
			if valid != test.valid {
				t.Fatalf("parseJournalStream(%q) valid = %v, want %v", test.raw, valid, test.valid)
			}
			if valid && (device != test.device || inode != test.inode) {
				t.Errorf("parseJournalStream(%q) = %d:%d, want %d:%d", test.raw, device, inode, test.device, test.inode)
			}
		})
	}
}
