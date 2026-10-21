package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArgRecordScript writes a herdr stand-in that appends its own argv to a
// file and answers every command with a successful agent start, so the test can
// read back exactly what would have reached the CLI.
func writeArgRecordScript(t *testing.T, dir string) (bin, record string) {
	t.Helper()
	record = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "herdr")
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + record + "\"\n" +
		"printf '%s\\n' '{\"result\":{\"type\":\"agent_started\",\"agent\":{\"pane_id\":\"pane-1\",\"running\":true}}}'\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write herdr script: %v", err)
	}
	return bin, record
}

func TestStartAgentPassesAgentArgsAfterSeparator(t *testing.T) {
	dir := t.TempDir()
	bin, record := writeArgRecordScript(t, dir)
	client := NewClient(bin, filepath.Join(dir, "herdr.sock"))

	if _, err := client.StartAgent(context.Background(), "project-hermes", "hermes", "pane-req", 30_000, "-p", "apollo"); err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}

	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	got := strings.TrimSpace(string(argv))
	want := "agent start project-hermes --kind hermes --pane pane-req --timeout 30000 -- -p apollo"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestStartAgentOmitsSeparatorWithoutAgentArgs(t *testing.T) {
	dir := t.TempDir()
	bin, record := writeArgRecordScript(t, dir)
	client := NewClient(bin, filepath.Join(dir, "herdr.sock"))

	if _, err := client.StartAgent(context.Background(), "project-codex", "codex", "pane-req", 30_000); err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}

	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	got := strings.TrimSpace(string(argv))
	want := "agent start project-codex --kind codex --pane pane-req --timeout 30000"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if strings.Contains(got, "-- ") || strings.HasSuffix(got, " --") {
		t.Fatalf("argv = %q, must not carry a separator with no agent args", got)
	}
}
