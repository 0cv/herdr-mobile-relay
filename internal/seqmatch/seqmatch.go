package seqmatch

// Match represents a matching block: a[i:i+Size] == b[j:j+Size].
type Match struct {
	A    int
	B    int
	Size int
}

// Matcher implements Python's difflib.SequenceMatcher with autojunk=False.
// It finds matching blocks between two sequences of strings using the
// Ratcliff/Obershelp gestalt pattern matching algorithm.
type Matcher struct {
	a   []string
	b   []string
	b2j map[string][]int
}

func NewMatcher(a, b []string) *Matcher {
	m := &Matcher{a: a, b: b}
	m.buildB2J()
	return m
}

func (m *Matcher) buildB2J() {
	m.b2j = make(map[string][]int, len(m.b))
	for j, elem := range m.b {
		m.b2j[elem] = append(m.b2j[elem], j)
	}
}

// FindLongestMatch finds the longest matching block in a[alo:ahi] and b[blo:bhi].
func (m *Matcher) FindLongestMatch(alo, ahi, blo, bhi int) Match {
	besti, bestj, bestsize := alo, blo, 0
	j2len := make(map[int]int)

	for i := alo; i < ahi; i++ {
		newj2len := make(map[int]int)
		for _, j := range m.b2j[m.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti = i - k + 1
				bestj = j - k + 1
				bestsize = k
			}
		}
		j2len = newj2len
	}

	return Match{A: besti, B: bestj, Size: bestsize}
}

// GetMatchingBlocks returns a list of Match triples describing matching
// subsequences, sorted by position, with a sentinel (len(a), len(b), 0) at the end.
// This is a faithful port of Python's difflib.SequenceMatcher.get_matching_blocks()
// with autojunk=False (no junk heuristic).
func (m *Matcher) GetMatchingBlocks() []Match {
	la, lb := len(m.a), len(m.b)

	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, la, 0, lb}}
	var blocks []Match

	for len(queue) > 0 {
		s := queue[len(queue)-1]
		queue = queue[:len(queue)-1]

		match := m.FindLongestMatch(s.alo, s.ahi, s.blo, s.bhi)
		if match.Size > 0 {
			blocks = append(blocks, match)
			if s.alo < match.A && s.blo < match.B {
				queue = append(queue, span{s.alo, match.A, s.blo, match.B})
			}
			if match.A+match.Size < s.ahi && match.B+match.Size < s.bhi {
				queue = append(queue, span{match.A + match.Size, s.ahi, match.B + match.Size, s.bhi})
			}
		}
	}

	sortMatches(blocks)

	// Merge adjacent blocks
	var merged []Match
	i1, j1, k1 := 0, 0, 0
	for _, m2 := range blocks {
		if i1+k1 == m2.A && j1+k1 == m2.B {
			k1 += m2.Size
		} else {
			if k1 > 0 {
				merged = append(merged, Match{i1, j1, k1})
			}
			i1, j1, k1 = m2.A, m2.B, m2.Size
		}
	}
	if k1 > 0 {
		merged = append(merged, Match{i1, j1, k1})
	}
	merged = append(merged, Match{la, lb, 0})
	return merged
}

func sortMatches(matches []Match) {
	// Insertion sort — blocks are typically few (< 50)
	for i := 1; i < len(matches); i++ {
		key := matches[i]
		j := i - 1
		for j >= 0 && compareMatch(matches[j], key) > 0 {
			matches[j+1] = matches[j]
			j--
		}
		matches[j+1] = key
	}
}

func compareMatch(a, b Match) int {
	if a.A != b.A {
		if a.A < b.A {
			return -1
		}
		return 1
	}
	if a.B != b.B {
		if a.B < b.B {
			return -1
		}
		return 1
	}
	return 0
}
