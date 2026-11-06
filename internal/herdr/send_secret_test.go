package herdr

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendSecretRuneBatch(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HERDR_TEST_ARGS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TEST_ARGS", record)
	c := NewClient(bin, filepath.Join(dir, "unused.sock"))
	if err := c.SendSecret(t.Context(), "p1", "É漢 A! "); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"pane", "send-keys", "p1", "É", "漢", " ", "A", "!", " ", "Enter", ""}, "\n")
	if string(got) != want {
		t.Fatal("secret rune batch differs")
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' 'private-marker' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = c.SendSecret(t.Context(), "p1", "private-marker")
	if !errors.Is(err, ErrDispatchedUnknown) || strings.Contains(err.Error(), "private-marker") {
		t.Fatal("secret error was not redacted")
	}
}
