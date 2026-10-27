package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewVersionProbeDoesNotInheritReleaseCredentials(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "received-secrets")
	t.Setenv("GH_TOKEN", "synthetic-probe-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-probe-token")
	t.Setenv("HERDR_GITHUB_TOKEN_FILE", "/tmp/synthetic-updater-credential")
	t.Setenv("REVIEW_RECORD", record)
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s|%s|%s' \"${GH_TOKEN:-}\" \"${GITHUB_TOKEN:-}\" \"${HERDR_GITHUB_TOKEN_FILE:-}\" > \"$REVIEW_RECORD\"\nprintf 'codex-cli 1.2.3\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := NewResolver(t.TempDir(), nil).AgentVersion("codex"); got != "1.2.3" {
		t.Fatalf("version = %q", got)
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Trim(string(data), "|") != "" {
		t.Fatalf("version probe received release credentials: %s", data)
	}
}
