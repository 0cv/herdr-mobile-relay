package activeruntime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAcceptsOneExactPrivateRuntimeSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "active-runtime.json")
	want := Snapshot{
		SchemaVersion:         1,
		Generation:            "generation-1",
		SessionName:           "generation-1",
		SocketPath:            filepath.Join(root, "sessions", "generation-1", "herdr.sock"),
		ExpectedInventoryPath: filepath.Join(root, "sessions", "generation-1", "expected-inventory.json"),
	}
	writeSnapshotFixture(t, path, `{"schemaVersion":1,"generation":"generation-1","sessionName":"generation-1","socketPath":"`+want.SocketPath+`","expectedInventoryPath":"`+want.ExpectedInventoryPath+`"}`)

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestLoadRejectsUnsafeRuntimeSnapshotFiles(t *testing.T) {
	valid := func(root string) string {
		return `{"schemaVersion":1,"generation":"g1","sessionName":"g1","socketPath":"` + filepath.Join(root, "sessions", "g1", "herdr.sock") + `","expectedInventoryPath":"` + filepath.Join(root, "sessions", "g1", "expected-inventory.json") + `"}`
	}
	t.Run("missing", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "missing"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing error = %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		_, err := Load(t.TempDir())
		if err == nil {
			t.Fatal("directory accepted")
		}
	})
	t.Run("broad mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "active-runtime.json")
		writeSnapshotFixture(t, path, valid(filepath.Dir(path)))
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("group-readable pointer accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		writeSnapshotFixture(t, target, valid(root))
		link := filepath.Join(root, "active-runtime.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(link); err == nil {
			t.Fatal("symlink pointer accepted")
		}
	})
	t.Run("same-inode symlink swap", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "active-runtime.json")
		writeSnapshotFixture(t, path, valid(root))
		moved := filepath.Join(root, "moved")
		ops := defaultLoadIO()
		ops.lstat = func(string) (os.FileInfo, error) {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if err := os.Rename(path, moved); err != nil {
				return nil, err
			}
			if err := os.Symlink(moved, path); err != nil {
				return nil, err
			}
			return info, nil
		}
		if _, err := loadWith(ops, path); err == nil {
			t.Fatal("same-inode symlink swap accepted")
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "active-runtime.json")
		writeSnapshotFixture(t, path, valid(root))
		if err := os.Link(path, filepath.Join(root, "second-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatal("hardlinked pointer accepted")
		}
	})
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "active-runtime.json")
		writeSnapshotFixture(t, path, strings.Repeat("x", maxSnapshotBytes+1))
		if _, err := Load(path); err == nil {
			t.Fatal("oversized pointer accepted")
		}
	})
}

func TestLoadRejectsEveryInvalidRuntimeSnapshotField(t *testing.T) {
	root := t.TempDir()
	absSocket := filepath.Join(root, "sessions", "g1", "herdr.sock")
	absInventory := filepath.Join(root, "sessions", "g1", "expected-inventory.json")
	valid := `{"schemaVersion":1,"generation":"g1","sessionName":"g1","socketPath":"` + absSocket + `","expectedInventoryPath":"` + absInventory + `"}`
	tests := map[string]string{
		"malformed":              `{`,
		"trailing value":         valid + `{}`,
		"trailing invalid":       valid + `nope`,
		"unknown field":          strings.TrimSuffix(valid, `}`) + `,"future":true}`,
		"wrong schema":           strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"missing generation":     strings.Replace(valid, `"generation":"g1",`, ``, 1),
		"blank generation":       strings.Replace(valid, `"generation":"g1"`, `"generation":" "`, 1),
		"control generation":     strings.Replace(valid, `"generation":"g1"`, `"generation":"g1\u000a"`, 1),
		"long generation":        strings.Replace(valid, `"generation":"g1"`, `"generation":"`+strings.Repeat("g", 257)+`"`, 1),
		"different session":      strings.Replace(valid, `"sessionName":"g1"`, `"sessionName":"other"`, 1),
		"missing session":        strings.Replace(valid, `,"sessionName":"g1"`, ``, 1),
		"relative socket":        strings.Replace(valid, absSocket, `relative/herdr.sock`, 1),
		"unnormalized socket":    strings.Replace(valid, absSocket, filepath.Join(root, "g1", "..", "g1", "herdr.sock"), 1),
		"missing socket":         strings.Replace(valid, `,"socketPath":"`+absSocket+`"`, ``, 1),
		"relative inventory":     strings.Replace(valid, absInventory, `relative/expected.json`, 1),
		"unnormalized inventory": strings.Replace(valid, absInventory, filepath.Join(root, "g1", "..", "g1", "expected.json"), 1),
		"missing inventory":      strings.Replace(valid, `,"expectedInventoryPath":"`+absInventory+`"`, ``, 1),
		"outside socket":         strings.Replace(valid, absSocket, filepath.Join(root, "outside", "herdr.sock"), 1),
		"socket prefix trick":    strings.Replace(valid, absSocket, filepath.Join(root, "sessions", "g1-evil", "herdr.sock"), 1),
		"socket sibling":         strings.Replace(valid, absSocket, filepath.Join(root, "sessions", "g2", "herdr.sock"), 1),
		"wrong socket basename":  strings.Replace(valid, absSocket, filepath.Join(root, "sessions", "g1", "other.sock"), 1),
		"outside inventory":      strings.Replace(valid, absInventory, filepath.Join(root, "outside", "expected-inventory.json"), 1),
		"inventory prefix trick": strings.Replace(valid, absInventory, filepath.Join(root, "sessions", "g1-evil", "expected-inventory.json"), 1),
		"inventory sibling":      strings.Replace(valid, absInventory, filepath.Join(root, "sessions", "g2", "expected-inventory.json"), 1),
		"wrong inventory name":   strings.Replace(valid, absInventory, filepath.Join(root, "sessions", "g1", "other.json"), 1),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, "active-runtime.json")
			writeSnapshotFixture(t, path, content)
			if _, err := Load(path); err == nil {
				t.Fatalf("invalid snapshot accepted: %s", content)
			}
		})
	}
}

func TestLoadPropagatesEveryOpenReadBoundaryFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "active-runtime.json")
	content := `{"schemaVersion":1,"generation":"g1","sessionName":"g1","socketPath":"` + filepath.Join(root, "sessions", "g1", "herdr.sock") + `","expectedInventoryPath":"` + filepath.Join(root, "sessions", "g1", "expected-inventory.json") + `"}`
	writeSnapshotFixture(t, path, content)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	writeSnapshotFixture(t, other, content)
	otherInfo, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func() loadIO{
		"open": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) { return nil, errors.New("open") }}
		},
		"stat": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) { return &fixtureSnapshotFile{statErr: errors.New("stat")}, nil }}
		},
		"unsafe after open": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) { return &fixtureSnapshotFile{info: directoryInfo}, nil }}
		},
		"changed after open": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) { return &fixtureSnapshotFile{info: otherInfo}, nil }}
		},
		"read": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) {
				return &fixtureSnapshotFile{info: info, readErr: errors.New("read")}, nil
			}}
		},
		"close": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) {
				return &fixtureSnapshotFile{info: info, reader: strings.NewReader(content), closeErr: errors.New("close")}, nil
			}}
		},
		"growth after stat": func() loadIO {
			return loadIO{lstat: func(string) (os.FileInfo, error) { return info, nil }, open: func(string) (snapshotFile, error) {
				return &fixtureSnapshotFile{info: info, reader: strings.NewReader(strings.Repeat("x", maxSnapshotBytes+1))}, nil
			}}
		},
	}
	for name, operation := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := loadWith(operation(), path); err == nil {
				t.Fatal("injected boundary failure was accepted")
			}
		})
	}
}

func TestRuntimeNameRejectsPathAndControlCharacters(t *testing.T) {
	for _, value := range []string{".", "..", "bad/name", `bad\name`, "bad\x7f"} {
		if validName(value) {
			t.Fatalf("unsafe generation accepted: %q", value)
		}
	}
}

type fixtureSnapshotFile struct {
	reader   io.Reader
	info     os.FileInfo
	statErr  error
	readErr  error
	closeErr error
}

func (f *fixtureSnapshotFile) Read(data []byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.reader == nil {
		return 0, io.EOF
	}
	return f.reader.Read(data)
}

func (f *fixtureSnapshotFile) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *fixtureSnapshotFile) Close() error               { return f.closeErr }

func writeSnapshotFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
