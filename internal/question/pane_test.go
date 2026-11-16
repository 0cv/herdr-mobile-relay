package question

import (
	"reflect"
	"strings"
	"testing"
)

func TestNewPaneCRLFMatchesLF(t *testing.T) {
	lf := "│ Question 1/2 │\n\x1b[1m  1. Yes  \x1b[0m\n  ┃  Pick one                    extra\n  ┃ plain\nunmarked  \n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	lfPane, crlfPane := newPane(lf), newPane(crlf)
	for name, pair := range map[string][2][]string{
		"trimmed":  {lfPane.trimmed(), crlfPane.trimmed()},
		"indented": {lfPane.indented(), crlfPane.indented()},
		"openCode": {lfPane.openCode(), crlfPane.openCode()},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Fatalf("%s differs between LF and CRLF:\nLF   %q\nCRLF %q", name, pair[0], pair[1])
		}
	}
	if got := lfPane.openCode()[2]; got != "Pick one" {
		t.Fatalf("openCode row = %q, want %q", got, "Pick one")
	}
}
