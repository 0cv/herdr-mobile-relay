package history

import (
	"os"
	"path/filepath"
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

func TestMergeLimitedReportsActualClipping(t *testing.T) {
	m := NewManager(t.TempDir())

	exact, clipped := m.MergeLimited("pane-1", "line1\nline2", 2)
	if clipped || exact != "line1\nline2" {
		t.Fatalf("exact result = %q, clipped = %v, want exact two-line output", exact, clipped)
	}

	limited, clipped := m.MergeLimited("pane-1", "line1\nline2", 1)
	if !clipped || limited != "line2" {
		t.Fatalf("limited result = %q, clipped = %v, want clipped final line", limited, clipped)
	}
}

func TestTailOverlapRefreshesANSIStyle(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", "old output\n\x1b[42mDeny\x1b[0m\nshared row"+footers)

	result := m.Merge("pane-1", "\x1b[44mDeny\x1b[0m\nshared row\nnew output"+footers)
	if !strings.Contains(result, "\x1b[44mDeny") {
		t.Fatalf("overlapping row retained stale ANSI style: %q", result)
	}
	if strings.Contains(result, "\x1b[42mDeny") {
		t.Fatalf("overlapping row retained old ANSI style: %q", result)
	}
}

func TestSequenceMatchRefreshesANSIStyle(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge(
		"pane-1",
		"context one\ncontext two\n\x1b[42mPermissions\x1b[0m\nold panel one\nold panel two"+footers,
	)

	result := m.Merge(
		"pane-1",
		"context one\ncontext two\n\x1b[44mPermissions\x1b[0m\nnew panel one\nnew panel two"+footers,
	)
	if !strings.Contains(result, "\x1b[44mPermissions") {
		t.Fatalf("matched row retained stale ANSI style: %q", result)
	}
	if strings.Contains(result, "\x1b[42mPermissions") {
		t.Fatalf("matched row retained old ANSI style: %q", result)
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

func TestReconcileRemovesHistoryLeftByEarlierProcess(t *testing.T) {
	dir := t.TempDir()
	first := NewManager(dir)
	frame := "line1\nline2\nline3\nline4\nline5\nline6\nf1\nf2\nf3\nf4\nf5\nf6"
	first.Merge("removed-pane", frame)
	first.Merge("active-pane", frame)
	first.SaveAll()

	restarted := NewManager(dir)
	restarted.Reconcile(map[string]bool{"active-pane": true})

	if _, err := os.Stat(filepath.Join(dir, "claude-history", "removed-pane.json")); !os.IsNotExist(err) {
		t.Fatalf("removed pane history still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "claude-history", "active-pane.json")); err != nil {
		t.Fatalf("active pane history was removed: %v", err)
	}
}

// A pane the relay never watched from the start has no rows to align against,
// so the harvested transcript becomes the whole history.
func TestSeedFillsEmptyHistory(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"

	m.Seed("pane-1", "old 1\nold 2\nold 3"+footers)
	content, _ := m.Content("pane-1", 100)
	if !strings.Contains(content, "old 1") || !strings.Contains(content, "f6") {
		t.Fatalf("seeded content = %q, want the harvested rows and footer", content)
	}
	if depth := m.Depth("pane-1"); depth != 3 {
		t.Fatalf("seeded depth = %d, want the three harvested body rows", depth)
	}
}

// The harvest ends at the screen the relay is already watching, so the rows it
// shares with the stored history must not appear twice.
func TestSeedPrependsOnlyOlderRows(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", "watched 1\nwatched 2\nwatched 3"+footers)

	m.Seed("pane-1", "old 1\nold 2\nwatched 1\nwatched 2\nwatched 3"+footers)
	content, _ := m.Content("pane-1", 100)
	if strings.Count(content, "watched 1") != 1 {
		t.Fatalf("overlapping row duplicated: %q", content)
	}
	rows := strings.Split(content, "\n")
	if rows[0] != "old 1" || rows[2] != "watched 1" {
		t.Fatalf("seeded order = %#v, want harvested rows beneath watched rows", rows[:3])
	}
	if depth := m.Depth("pane-1"); depth != 5 {
		t.Fatalf("seeded depth = %d, want 2 harvested + 3 watched rows", depth)
	}
}

// Styled rows the relay watched outrank the harvest, which loses ANSI.
func TestSeedKeepsWatchedStyling(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", "\x1b[42mApprove\x1b[0m\nwatched tail"+footers)

	m.Seed("pane-1", "old 1\nApprove\nwatched tail"+footers)
	content, _ := m.Content("pane-1", 100)
	if !strings.Contains(content, "\x1b[42mApprove") {
		t.Fatalf("seed replaced the styled row: %q", content)
	}
	if strings.Count(content, "Approve") != 1 {
		t.Fatalf("styled row duplicated by its plain harvest: %q", content)
	}
}

// A pane read at one width and harvested at another reflows its own output, so
// only part of the overlap survives verbatim; the partial match still places the
// seam.
func TestSeedAlignsOnPartialMatch(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", "shared a\nshared b\nreflowed at phone width"+footers)

	m.Seed("pane-1", "old 1\nold 2\nshared a\nshared b\nreflowed at desktop width"+footers)
	content, _ := m.Content("pane-1", 100)
	if strings.Count(content, "shared a") != 1 {
		t.Fatalf("matched row duplicated: %q", content)
	}
	if !strings.Contains(content, "old 1") {
		t.Fatalf("older rows dropped: %q", content)
	}
	if strings.Contains(content, "reflowed at desktop width") {
		t.Fatalf("seed kept its own copy of a row the relay already watched: %q", content)
	}
}

// Nothing older than the stored history means nothing to prepend.
func TestSeedIsIdempotent(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	seed := "old 1\nold 2\nold 3" + footers
	m.Seed("pane-1", seed)
	first := m.Depth("pane-1")

	m.Seed("pane-1", seed)
	if second := m.Depth("pane-1"); second != first {
		t.Fatalf("second seed changed depth %d -> %d", first, second)
	}
}

// Live frames keep appending after a seed, and the seeded rows stay beneath them.
func TestSeedThenMergeKeepsBothEnds(t *testing.T) {
	m := NewManager(t.TempDir())
	footers := "\nf1\nf2\nf3\nf4\nf5\nf6"
	m.Merge("pane-1", "watched 1\nwatched 2"+footers)
	m.Seed("pane-1", "old 1\nwatched 1\nwatched 2"+footers)

	content, _ := m.MergeLimited("pane-1", "watched 2\nwatched 3"+footers, 100)
	for _, want := range []string{"old 1", "watched 1", "watched 3"} {
		if !strings.Contains(content, want) {
			t.Fatalf("merged content lost %q: %q", want, content)
		}
	}
	if strings.Count(content, "watched 2") != 1 {
		t.Fatalf("row duplicated across seed and merge: %q", content)
	}
}
