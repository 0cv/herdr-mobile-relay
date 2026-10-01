package herdr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentChildDoesNotInheritReleaseCredentials(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	t.Setenv("GH_TOKEN", "synthetic-download-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-download-token")
	t.Setenv("HERDR_GITHUB_TOKEN_FILE", filepath.Join(dir, "private-updater-token"))
	t.Setenv("HTTPS_PROXY", "http://proxy.example.test")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	script := "#!/bin/sh\n[ -z \"${GH_TOKEN+x}${GITHUB_TOKEN+x}${HERDR_GITHUB_TOKEN_FILE+x}\" ] || exit 1\n[ \"$HTTPS_PROXY\" = http://proxy.example.test ] || exit 2\n[ -n \"$CLAUDE_CONFIG_DIR\" ] || exit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := NewClient(bin, filepath.Join(dir, "sock"))
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Prompt(context.Background(), "pane", "hello"); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("HERDR_GITHUB_TOKEN_FILE") == "" {
		t.Fatal("updater lost its credential file")
	}
}
