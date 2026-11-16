package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTailFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readTailEntries(t *testing.T, path, agent string) []Entry {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := readJSONLTail(file, agent, "revision", info.Size())
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestJSONLTailOfLargeFileSkipsPartialFirstRecord(t *testing.T) {
	var content strings.Builder
	var starts []int
	padding := strings.Repeat("x", 1000)
	for index := 0; content.Len() < maxConversationBytes+1024*1024; index++ {
		starts = append(starts, content.Len())
		fmt.Fprintf(&content, `{"type":"assistant","message":{"content":"row-%06d %s"}}`+"\n", index, padding)
	}
	windowStart := content.Len() - maxConversationBytes
	firstWhole := 0
	for starts[firstWhole] < windowStart {
		firstWhole++
	}
	path := writeTailFile(t, content.String())

	entries := readTailEntries(t, path, "claude")
	if len(entries) != len(starts)-firstWhole {
		t.Fatalf("entries = %d, want %d", len(entries), len(starts)-firstWhole)
	}
	if want := fmt.Sprintf("row-%06d", firstWhole); !strings.HasPrefix(entries[0].Text, want) {
		t.Fatalf("first entry = %.20q, want prefix %q", entries[0].Text, want)
	}
	if want := fmt.Sprintf("row-%06d", len(starts)-1); !strings.HasPrefix(entries[len(entries)-1].Text, want) {
		t.Fatalf("last entry = %.20q, want prefix %q", entries[len(entries)-1].Text, want)
	}
}

func TestReadLatestKeepsUnterminatedLastRecordAndIgnoresUnfinishedOne(t *testing.T) {
	first := `{"type":"message","message":{"role":"user","content":"first"}}`
	last := `{"type":"message","message":{"role":"assistant","content":"last"}}`
	for name, test := range map[string]struct {
		content string
		want    []string
	}{
		"unterminated complete record": {first + "\n" + last, []string{"first", "last"}},
		"unfinished record":            {first + "\n" + `{"type":"message","message":{"role":"assistant","content":"la`, []string{"first"}},
	} {
		t.Run(name, func(t *testing.T) {
			reader, home := testReader(t)
			path := filepath.Join(home, ".pi", "agent", "sessions", "--work--", "2026-01-01T00-00-00-000Z_"+testSessionID+".jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			entries, err := reader.ReadLatest("pi", ProjectContext{CWD: "/work"}, testSessionID, 10)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, entry := range entries {
				got = append(got, entry.Text)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("entries = %v, want %v", got, test.want)
			}
		})
	}
}

func TestClaudeSegmentReadsOnlyUpToItsCapturedEndAndRejectsRewrites(t *testing.T) {
	reader, home := testReader(t)
	root := filepath.Join(home, ".claude", "projects", "-work")
	path := filepath.Join(root, testSessionID+".jsonl")
	writeRows(t, path, map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "captured"}})
	location := reader.LocateWithProject("claude", ProjectContext{CWD: "/work"}, testSessionID)
	chain, err := resolveClaudeChain(context.Background(), location, testSessionID)
	if err != nil || len(chain.Segments) != 1 {
		t.Fatalf("chain = %#v, err=%v", chain, err)
	}
	segment := chain.Segments[0]

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"assistant","uuid":"a2","message":{"content":"appended"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := segment.readEntries(maxConversationBytes)
	if err != nil || len(entries) != 1 || entries[0].Text != "captured" {
		t.Fatalf("entries after append = %#v, err=%v", entries, err)
	}

	if err := os.WriteFile(path, []byte(`{"type":"assistant","uuid":"a1","message":{"content":"rewritten"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := segment.readEntries(maxConversationBytes); err == nil {
		t.Fatal("rewritten segment was read")
	}
}
