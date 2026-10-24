package slashcmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogReaderReadLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	paddedFile(t, path, "---\nname: large\ndescription: Metadata only\n---\n", maxCommandFileSize)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := newCatalogReader()
	data, ok := reader.readBoundedCommandFile(file)
	if !ok || len(data) != maxMetadataSize+1 {
		t.Fatalf("prefix bytes = %d, ok = %v", len(data), ok)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil || offset != int64(len(data)) {
		t.Fatalf("file offset = %d, err = %v", offset, err)
	}
	if reader.remaining != maxCatalogReadSize-int64(len(data)) {
		t.Fatalf("remaining bytes = %d", reader.remaining)
	}
	reads := 1
	for {
		_, ok := reader.readSkillMetadata(path)
		if !ok {
			break
		}
		reads++
	}
	if reads != maxCatalogReadSize/(maxMetadataSize+1) || !reader.truncated {
		t.Fatalf("reads = %d, truncated = %v", reads, reader.truncated)
	}
	remaining := reader.remaining
	if _, ok := reader.readSkillMetadata(path); ok || reader.remaining != remaining {
		t.Fatal("reader continued after aggregate exhaustion")
	}
}

func TestCatalogReaderSettingsShareBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	paddedFile(t, path, "{}", maxSettingsSize)
	reader := newCatalogReader()
	for range maxCatalogReadSize / maxSettingsSize {
		data, ok := reader.readPath(path, maxSettingsSize, maxSettingsSize+1)
		if !ok || len(data) != maxSettingsSize {
			t.Fatalf("settings bytes = %d, ok = %v", len(data), ok)
		}
	}
	if !reader.truncated || reader.remaining != 0 {
		t.Fatalf("remaining = %d, truncated = %v", reader.remaining, reader.truncated)
	}
	if _, ok := reader.readCommandFile(path); ok {
		t.Fatal("metadata read escaped the settings budget")
	}
}

func TestMetadataPrefixBoundaries(t *testing.T) {
	for _, parse := range []struct {
		name string
		fn   func([]byte) (map[string]string, bool)
	}{
		{"shared", parseFrontmatterBytes},
		{"cursor", parseCursorSkillMetadata},
	} {
		t.Run(parse.name, func(t *testing.T) {
			for _, closed := range []bool{false, true} {
				prefix := "---\nname: bounded\ndescription: Metadata\n"
				content := prefix + strings.Repeat("\n", maxMetadataSize-len(prefix))
				if closed {
					content = content[:len(content)-4] + "---\n"
				}
				metadata, ok := parse.fn([]byte(content))
				if !ok || metadata["name"] != "bounded" {
					t.Fatalf("at-limit metadata = %v, ok = %v, closed = %v", metadata, ok, closed)
				}
				metadata, ok = parse.fn([]byte(content + strings.Repeat("x\n", maxCommandFileSize)))
				if ok != closed || !closed && metadata != nil {
					t.Fatalf("beyond-limit metadata accepted = %v, closed = %v", ok, closed)
				}
			}
		})
	}
}
