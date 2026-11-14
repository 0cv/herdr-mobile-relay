package conversation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSourceStampCompare(t *testing.T) {
	const content = "0123456789"
	modTime := time.Unix(1_700_000_000, 0)

	write := func(t *testing.T, path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatal(err)
		}
	}
	replaceWith := func(t *testing.T, path, text string) {
		t.Helper()
		replacement := path + ".new"
		write(t, replacement, text)
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		forget func(*sourceStamp)
		mutate func(t *testing.T, path string)
		want   sourceChange
	}{
		{name: "untouched", mutate: func(*testing.T, string) {}, want: sourceUnchanged},
		{name: "appended", mutate: func(t *testing.T, path string) {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString("more"); err != nil {
				t.Fatal(err)
			}
		}, want: sourceGrown},
		{name: "truncated", mutate: func(t *testing.T, path string) { write(t, path, content[:4]) }, want: sourceChanged},
		{name: "rewritten at the same size with the modification time restored", mutate: func(t *testing.T, path string) { write(t, path, "abcdefghij") }, want: sourceChanged},
		{name: "rewritten at the same size when no change token was recorded", forget: func(stamp *sourceStamp) { stamp.changeToken = "" }, mutate: func(t *testing.T, path string) { write(t, path, "abcdefghij") }, want: sourceUnchanged},
		{name: "modification time moved", mutate: func(t *testing.T, path string) {
			if err := os.Chtimes(path, modTime, modTime.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}, want: sourceChanged},
		{name: "modification time moved when none was recorded", forget: func(stamp *sourceStamp) { stamp.modTime = 0; stamp.changeToken = "" }, mutate: func(t *testing.T, path string) {
			if err := os.Chtimes(path, modTime, modTime.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}, want: sourceUnchanged},
		{name: "replaced by another file", mutate: func(t *testing.T, path string) { replaceWith(t, path, content) }, want: sourceChanged},
		{name: "replaced by another file when no identity was recorded", forget: func(stamp *sourceStamp) { stamp.identity = ""; stamp.changeToken = "" }, mutate: func(t *testing.T, path string) { replaceWith(t, path, content) }, want: sourceUnchanged},
		{name: "grown and replaced", mutate: func(t *testing.T, path string) { replaceWith(t, path, content+"more") }, want: sourceChanged},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.jsonl")
			write(t, path, content)
			stamp := stampOfInfo(statFile(t, path))
			if test.forget != nil {
				test.forget(&stamp)
			}
			test.mutate(t, path)
			if got := stamp.compare(statFile(t, path)); got != test.want {
				t.Fatalf("compare = %v, want %v", got, test.want)
			}
		})
	}
}

func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
