package history

import (
	"strings"
	"testing"
)

func TestTailOverlapAppend(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// First frame
	frame1 := "line1\nline2\nline3\nline4\nline5\nline6\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6"
	result := m.Merge("pane-1", frame1)
	if !strings.Contains(result, "line1") {
		t.Error("first merge should contain line1")
	}

	// Second frame overlaps: last 3 lines of history == first 3 of new frame
	frame2 := "line4\nline5\nline6\nline7\nline8\nline9\nfooter1\nfooter2\nfooter3\nfooter4\nfooter5\nfooter6"
	result = m.Merge("pane-1", frame2)

	if !strings.Contains(result, "line1") {
		t.Error("should still contain line1 from history")
	}
	if !strings.Contains(result, "line9") {
		t.Error("should contain new line9")
	}
	// Should not duplicate line4-6
	count := strings.Count(result, "line4")
	if count != 1 {
		t.Errorf("line4 appears %d times, want 1", count)
	}
}

func TestNoOverlapAppendsWhole(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	frame1 := "aaa\nbbb\nccc\nddd\neee\nfff\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", frame1)

	frame2 := "xxx\nyyy\nzzz\nwww\nvvv\nuuu\nf1\nf2\nf3\nf4\nf5\nf6"
	result := m.Merge("pane-1", frame2)

	if !strings.Contains(result, "aaa") {
		t.Error("should contain original history")
	}
	if !strings.Contains(result, "xxx") {
		t.Error("should contain new frame content")
	}
}

func TestRepetitiveContentDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// Build a history with repeated lines
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "repeated output line")
	}
	lines = append(lines, "unique-marker-1")
	for i := 0; i < 10; i++ {
		lines = append(lines, "after unique")
	}
	frame1 := strings.Join(lines, "\n") + "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", frame1)

	// New frame starts with some of the repeated lines plus new content
	var newLines []string
	for i := 0; i < 5; i++ {
		newLines = append(newLines, "after unique")
	}
	newLines = append(newLines, "brand new content")
	newLines = append(newLines, "more new content")
	frame2 := strings.Join(newLines, "\n") + "\nf1\nf2\nf3\nf4\nf5\nf6"
	result := m.Merge("pane-1", frame2)

	if !strings.Contains(result, "unique-marker-1") {
		t.Error("should preserve unique marker from history")
	}
	if !strings.Contains(result, "brand new content") {
		t.Error("should contain new content")
	}
}

func TestUnchangedFrameNoOp(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	frame := "line1\nline2\nline3\nline4\nline5\nline6\nf1\nf2\nf3\nf4\nf5\nf6"
	result1 := m.Merge("pane-1", frame)
	result2 := m.Merge("pane-1", frame)

	if result1 != result2 {
		t.Error("unchanged frame should produce identical result")
	}
}

func TestMaxLinesTrimming(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	// Feed more than MaxLines
	var lines []string
	for i := 0; i < MaxLines+100; i++ {
		lines = append(lines, "line")
	}
	frame := strings.Join(lines, "\n") + "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", frame)

	m.mu.Lock()
	state := m.states["pane-1"]
	m.mu.Unlock()

	if len(state.History) > MaxLines {
		t.Errorf("history len = %d, exceeds max %d", len(state.History), MaxLines)
	}
}

func TestNormalizeLine(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"\x1b[32mgreen text\x1b[0m", "green text"},
		{"hello\r\n", "hello"},
		{"trailing spaces   ", "trailing spaces"},
		{"\x1b[1;34mbold blue\x1b[0m normal", "bold blue normal"},
	}
	for _, tt := range tests {
		got := NormalizeLine(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeLine(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDiscard(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	frame := "line1\nline2\nline3\nline4\nline5\nline6\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", frame)
	m.Discard("pane-1")

	m.mu.Lock()
	_, exists := m.states["pane-1"]
	m.mu.Unlock()
	if exists {
		t.Error("state should be discarded")
	}
}
