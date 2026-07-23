package seqmatch

import (
	"testing"
)

func TestIdenticalSequences(t *testing.T) {
	a := []string{"hello", "world", "foo"}
	m := NewMatcher(a, a)
	blocks := m.GetMatchingBlocks()

	if len(blocks) != 2 { // one real block + sentinel
		t.Fatalf("expected 2 blocks, got %d: %v", len(blocks), blocks)
	}
	if blocks[0].A != 0 || blocks[0].B != 0 || blocks[0].Size != 3 {
		t.Errorf("block[0] = %v, want {0,0,3}", blocks[0])
	}
	if blocks[1].A != 3 || blocks[1].B != 3 || blocks[1].Size != 0 {
		t.Errorf("sentinel = %v, want {3,3,0}", blocks[1])
	}
}

func TestNoMatch(t *testing.T) {
	a := []string{"aaa", "bbb"}
	b := []string{"ccc", "ddd"}
	m := NewMatcher(a, b)
	blocks := m.GetMatchingBlocks()

	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (sentinel), got %d", len(blocks))
	}
	if blocks[0].Size != 0 {
		t.Errorf("sentinel size = %d", blocks[0].Size)
	}
}

func TestPartialOverlap(t *testing.T) {
	a := []string{"a", "b", "c", "d", "e"}
	b := []string{"c", "d", "e", "f", "g"}
	m := NewMatcher(a, b)
	blocks := m.GetMatchingBlocks()

	// Should find c,d,e as a matching block
	found := false
	for _, bl := range blocks {
		if bl.Size >= 3 && a[bl.A] == "c" && b[bl.B] == "c" {
			found = true
			if bl.Size != 3 {
				t.Errorf("match size = %d, want 3", bl.Size)
			}
		}
	}
	if !found {
		t.Error("did not find expected c,d,e match")
	}
}

func TestRepetitiveContent(t *testing.T) {
	// Adversarial: same line repeated many times
	a := make([]string, 20)
	b := make([]string, 20)
	for i := range a {
		a[i] = "repeated line"
		b[i] = "repeated line"
	}
	// Offset b by 5
	b = append(b[5:], "new1", "new2", "new3", "new4", "new5")

	m := NewMatcher(a, b)
	blocks := m.GetMatchingBlocks()

	// Should find a large matching block
	var largest Match
	for _, bl := range blocks {
		if bl.Size > largest.Size {
			largest = bl
		}
	}
	if largest.Size < 10 {
		t.Errorf("largest match = %d, expected >= 10 for repetitive content", largest.Size)
	}
}

func TestRepetitiveWithUniqueAnchor(t *testing.T) {
	// History has repeated content, then a unique line, then more repeated
	a := []string{"x", "x", "x", "UNIQUE", "x", "x", "x", "y", "z"}
	b := []string{"x", "x", "UNIQUE", "x", "x", "x", "y", "z", "new"}

	m := NewMatcher(a, b)
	blocks := m.GetMatchingBlocks()

	// The matcher should anchor on the UNIQUE line and the y,z tail
	var totalMatched int
	for _, bl := range blocks {
		totalMatched += bl.Size
	}
	if totalMatched < 6 {
		t.Errorf("total matched = %d, expected >= 6", totalMatched)
	}
}

func TestEmptySequences(t *testing.T) {
	m := NewMatcher(nil, nil)
	blocks := m.GetMatchingBlocks()
	if len(blocks) != 1 || blocks[0].Size != 0 {
		t.Errorf("empty sequences: got %v", blocks)
	}

	m2 := NewMatcher([]string{"a"}, nil)
	blocks2 := m2.GetMatchingBlocks()
	if len(blocks2) != 1 || blocks2[0].Size != 0 {
		t.Errorf("one empty: got %v", blocks2)
	}
}

func TestFindLongestMatch(t *testing.T) {
	a := []string{"a", "b", "c", "d"}
	b := []string{"x", "b", "c", "y"}
	m := NewMatcher(a, b)

	match := m.FindLongestMatch(0, len(a), 0, len(b))
	if match.A != 1 || match.B != 1 || match.Size != 2 {
		t.Errorf("FindLongestMatch = %v, want {1,1,2}", match)
	}
}

func TestMergedAdjacentBlocks(t *testing.T) {
	// Two adjacent matches should be merged into one
	a := []string{"a", "b", "c"}
	b := []string{"a", "b", "c"}
	m := NewMatcher(a, b)
	blocks := m.GetMatchingBlocks()

	// Should be merged into a single block of size 3
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (merged + sentinel), got %d", len(blocks))
	}
	if blocks[0].Size != 3 {
		t.Errorf("merged size = %d, want 3", blocks[0].Size)
	}
}
