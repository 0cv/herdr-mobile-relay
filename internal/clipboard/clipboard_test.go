package clipboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReaderPrefersWaylandClipboard(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "wl-paste", "printf wayland")
	writeExecutable(t, binDir, "xsel", "printf x11")
	writeExecutable(t, binDir, "wl-copy", ":")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resetForTest()
	t.Cleanup(resetForTest)

	name, read, ok := Reader()
	if !ok || name != "wl-paste" || read == nil {
		t.Fatalf("Reader() = (%q, %v, %v), want wl-paste reader", name, read != nil, ok)
	}
	content, err := read(context.Background())
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if string(content) != "wayland" {
		t.Fatalf("read() = %q, want wayland", content)
	}
}

func TestReaderKeepsBackendForEmptyClipboard(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "wl-paste", "exit 1")
	writeExecutable(t, binDir, "wl-copy", ":")
	t.Setenv("PATH", binDir)
	resetForTest()
	t.Cleanup(resetForTest)

	name, read, ok := Reader()
	if !ok || name != "wl-paste" || read == nil {
		t.Fatalf("Reader() = (%q, %v, %v), want discovered wl-paste backend", name, read != nil, ok)
	}
	if _, err := read(context.Background()); err == nil {
		t.Fatal("read() succeeded for the simulated empty clipboard")
	}
}

func TestReaderPairsReaderAndWriterBackends(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "wl-paste", "printf wayland")
	writeExecutable(t, binDir, "xsel", "if [ \"$1 $2\" = \"-b -o\" ]; then printf x11; else /bin/cat >/dev/null; fi")
	t.Setenv("PATH", binDir)
	resetForTest()
	t.Cleanup(resetForTest)

	name, read, ok := Reader()
	if !ok || name != "xsel" || read == nil {
		t.Fatalf("Reader() = (%q, %v, %v), want matched xsel backend", name, read != nil, ok)
	}
	content, err := read(context.Background())
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if string(content) != "x11" {
		t.Fatalf("read() = %q, want x11", content)
	}
}

func TestWriterUsesXselWhenWaylandIsAbsent(t *testing.T) {
	binDir := t.TempDir()
	output := filepath.Join(binDir, "clipboard.out")
	writeExecutable(t, binDir, "xsel", "if [ \"$1 $2\" = \"-b -o\" ]; then printf old; else /bin/cat >\"$CLIPBOARD_OUTPUT\"; fi")
	t.Setenv("PATH", binDir)
	t.Setenv("CLIPBOARD_OUTPUT", output)
	resetForTest()
	t.Cleanup(resetForTest)

	name, read, ok := Reader()
	if !ok || name != "xsel" || read == nil {
		t.Fatalf("Reader() = (%q, %v, %v), want xsel reader", name, read != nil, ok)
	}
	if err := Write(context.Background(), []byte("replacement")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	written, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read written clipboard: %v", err)
	}
	if string(written) != "replacement" {
		t.Fatalf("written clipboard = %q, want replacement", written)
	}
}

func TestReaderRequiresClipboardWriter(t *testing.T) {
	binDir := t.TempDir()
	writeExecutable(t, binDir, "pbpaste", "printf read-only")
	t.Setenv("PATH", binDir)
	resetForTest()
	t.Cleanup(resetForTest)

	name, read, ok := Reader()
	if name != "" || read != nil || ok {
		t.Fatalf("Reader() = (%q, %v, %v), want unavailable without pbcopy", name, read != nil, ok)
	}
}

func TestReaderReportsUnavailableTooling(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resetForTest()
	t.Cleanup(resetForTest)

	name, read, ok := Reader()
	if name != "" || read != nil || ok {
		t.Fatalf("Reader() = (%q, %v, %v), want unavailable", name, read != nil, ok)
	}
	if err := Write(context.Background(), []byte("data")); err == nil {
		t.Fatal("Write() succeeded without clipboard tooling")
	}
}

func writeExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+strings.TrimSpace(body)+"\n"), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
