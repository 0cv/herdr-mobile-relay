package testfixture

import (
	"slices"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

func ReplaceRow(t testing.TB, frame, text, replacement string) string {
	t.Helper()
	rows := strings.Split(frame, "\n")
	for i, row := range rows {
		if strings.Trim(rowText(row), " │") == text {
			rows[i] = replacement
			return strings.Join(rows, "\n")
		}
	}
	t.Fatalf("missing fixture row %q", text)
	return frame
}

func StaleOMP(t testing.TB, frame string) string {
	t.Helper()
	rows := strings.Split(frame, "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		row := rowText(rows[i])
		if strings.HasPrefix(row, "╰─") && strings.HasSuffix(row, "─╯") {
			return strings.Join(slices.Insert(rows, i+1, "The agent returned to its main task."), "\n")
		}
	}
	t.Fatal("missing OMP dialog border")
	return frame
}

func rowText(row string) string {
	return strings.TrimSpace(textutil.StripANSI(row))
}
