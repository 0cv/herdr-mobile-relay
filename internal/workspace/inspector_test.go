package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runTestGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = root
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestWorkspaceFileReadsStayInsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package main\n")
	writeTestFile(t, outside, "secret\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked.txt")); err != nil {
		t.Fatal(err)
	}

	preview, err := ReadFile(root, "src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Kind != "text" || preview.Text != "package main\n" {
		t.Fatalf("preview = %#v", preview)
	}
	for _, path := range []string{"../outside.txt", "/etc/passwd", "linked.txt"} {
		if _, err := ReadFile(root, path); err == nil {
			t.Fatalf("ReadFile(%q) escaped or followed a symlink", path)
		}
	}

	tree, err := TreeFor(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range tree.Entries {
		if entry.Path == "linked.txt" {
			t.Fatal("workspace tree exposed a symlink")
		}
	}
}

func TestWorkspaceTreeSkipsGeneratedDirectories(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "src", "app.ts"), "export {};\n")
	writeTestFile(t, filepath.Join(root, "node_modules", "package", "index.js"), "ignored\n")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "ignored\n")

	tree, err := TreeFor(root)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(tree.Entries))
	for _, entry := range tree.Entries {
		paths = append(paths, entry.Path)
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, "src/app.ts") || strings.Contains(joined, "node_modules") || strings.Contains(joined, ".git") {
		t.Fatalf("tree paths = %q", joined)
	}
}

func TestGitInspectionHandlesUntrackedAndDeletedFiles(t *testing.T) {
	root := t.TempDir()
	runTestGit(t, root, "init", "--quiet")
	writeTestFile(t, filepath.Join(root, "tracked.txt"), "tracked line\n")
	runTestGit(t, root, "add", "tracked.txt")
	runTestGit(t, root,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.invalid",
		"commit", "--quiet", "-m", "initial",
	)
	if err := os.Remove(filepath.Join(root, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "new.txt"), "new line\n")

	status, err := GitStatusFor(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available {
		t.Fatal("initialized repository reported unavailable")
	}
	statuses := make(map[string]string)
	for _, file := range status.Files {
		statuses[file.Path] = file.Status
	}
	if !strings.Contains(statuses["tracked.txt"], "D") || statuses["new.txt"] != "??" {
		t.Fatalf("statuses = %#v", statuses)
	}

	deleted, err := GitDiffFor(context.Background(), root, "tracked.txt")
	if err != nil {
		t.Fatalf("deleted diff: %v", err)
	}
	if !strings.Contains(deleted.Diff, "-tracked line") {
		t.Fatalf("deleted diff = %q", deleted.Diff)
	}
	untracked, err := GitDiffFor(context.Background(), root, "new.txt")
	if err != nil {
		t.Fatalf("untracked diff: %v", err)
	}
	if !strings.Contains(untracked.Diff, "+new line") {
		t.Fatalf("untracked diff = %q", untracked.Diff)
	}
	if _, err := GitDiffFor(context.Background(), root, "unchanged.txt"); err == nil {
		t.Fatal("diff for a path outside Git status unexpectedly succeeded")
	}
}
