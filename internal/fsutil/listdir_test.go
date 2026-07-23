package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListDirectoriesHome(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "projects"), 0o755)
	os.MkdirAll(filepath.Join(home, "docs"), 0o755)
	os.MkdirAll(filepath.Join(home, ".hidden"), 0o755)
	os.WriteFile(filepath.Join(home, "file.txt"), []byte("x"), 0o644)

	result := ListDirectories("", home)

	if result.Current.Path != home {
		t.Errorf("current.path = %q, want %q", result.Current.Path, home)
	}
	if result.Current.Label != "~" {
		t.Errorf("current.label = %q, want ~", result.Current.Label)
	}
	if result.Parent != "" {
		t.Errorf("parent = %q, want empty at home", result.Parent)
	}
	if len(result.Directories) != 2 {
		t.Fatalf("dirs = %d, want 2 (projects, docs)", len(result.Directories))
	}
	// Sorted case-insensitively
	if result.Directories[0].Name != "docs" {
		t.Errorf("first dir = %q, want docs", result.Directories[0].Name)
	}
	if result.Directories[1].Name != "projects" {
		t.Errorf("second dir = %q, want projects", result.Directories[1].Name)
	}
}

func TestListDirectoriesSubdir(t *testing.T) {
	home := t.TempDir()
	sub := filepath.Join(home, "projects")
	os.MkdirAll(filepath.Join(sub, "app1"), 0o755)
	os.MkdirAll(filepath.Join(sub, "app2"), 0o755)

	result := ListDirectories(sub, home)

	if result.Current.Path != sub {
		t.Errorf("current.path = %q", result.Current.Path)
	}
	if result.Current.Label != "~/projects" {
		t.Errorf("label = %q", result.Current.Label)
	}
	if result.Parent != home {
		t.Errorf("parent = %q, want %q", result.Parent, home)
	}
	if len(result.Directories) != 2 {
		t.Fatalf("dirs = %d, want 2", len(result.Directories))
	}
}

func TestListDirectoriesTraversalBlocked(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "safe"), 0o755)

	// Attempt traversal above home
	result := ListDirectories(filepath.Join(home, "..", ".."), home)
	if result.Current.Path != home {
		t.Errorf("traversal should fall back to home, got %q", result.Current.Path)
	}
}

func TestListDirectoriesNonexistentFallsBack(t *testing.T) {
	home := t.TempDir()
	result := ListDirectories("/nonexistent/path/xyz", home)
	if result.Current.Path != home {
		t.Errorf("nonexistent path should fall back to home, got %q", result.Current.Path)
	}
}

func TestListDirectoriesRelativePath(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "work"), 0o755)

	result := ListDirectories("work", home)
	if result.Current.Path != filepath.Join(home, "work") {
		t.Errorf("relative path not resolved: %q", result.Current.Path)
	}
}

func TestDisplayPath(t *testing.T) {
	tests := []struct {
		path, home, want string
	}{
		{"/home/user", "/home/user", "~"},
		{"/home/user/projects", "/home/user", "~/projects"},
		{"/other/path", "/home/user", "/other/path"},
	}
	for _, tt := range tests {
		got := displayPath(tt.path, tt.home)
		if got != tt.want {
			t.Errorf("displayPath(%q, %q) = %q, want %q", tt.path, tt.home, got, tt.want)
		}
	}
}
