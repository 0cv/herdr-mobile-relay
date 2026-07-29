package panedelta

import "strings"

const (
	minimumCopyLines = 3
	maxCandidates    = 64
)

type Segment struct {
	CopyStart int    `json:"copy_start,omitempty"`
	CopyLines int    `json:"copy_lines,omitempty"`
	Text      string `json:"text,omitempty"`
}

type lineKey [minimumCopyLines]string

func Build(previous, current string) []Segment {
	previousLines := strings.SplitAfter(previous, "\n")
	currentLines := strings.SplitAfter(current, "\n")
	matches := make(map[lineKey][]int, len(previousLines))
	for index := 0; index+minimumCopyLines <= len(previousLines); index++ {
		key := keyAt(previousLines, index)
		if len(matches[key]) < maxCandidates {
			matches[key] = append(matches[key], index)
		}
	}

	segments := make([]Segment, 0, 8)
	literalStart := 0
	flushLiteral := func(end int) {
		if end <= literalStart {
			return
		}
		segments = append(segments, Segment{Text: strings.Join(currentLines[literalStart:end], "")})
	}
	for currentIndex := 0; currentIndex < len(currentLines); {
		if currentIndex+minimumCopyLines > len(currentLines) {
			break
		}
		bestStart, bestLines := 0, 0
		for _, previousIndex := range matches[keyAt(currentLines, currentIndex)] {
			matched := matchingLines(previousLines, currentLines, previousIndex, currentIndex)
			if matched > bestLines {
				bestStart, bestLines = previousIndex, matched
			}
		}
		if bestLines < minimumCopyLines {
			currentIndex++
			continue
		}
		flushLiteral(currentIndex)
		segments = append(segments, Segment{CopyStart: bestStart, CopyLines: bestLines})
		currentIndex += bestLines
		literalStart = currentIndex
	}
	flushLiteral(len(currentLines))
	return segments
}

func Efficient(segments []Segment, current string) bool {
	literalBytes := 0
	for _, segment := range segments {
		literalBytes += len(segment.Text)
	}
	return literalBytes+len(segments)*64 < len(current)*3/4
}

func Apply(previous string, segments []Segment) (string, bool) {
	lines := strings.SplitAfter(previous, "\n")
	var output strings.Builder
	for _, segment := range segments {
		if segment.CopyLines > 0 {
			end := segment.CopyStart + segment.CopyLines
			if segment.CopyStart < 0 || end < segment.CopyStart || end > len(lines) {
				return "", false
			}
			for _, line := range lines[segment.CopyStart:end] {
				output.WriteString(line)
			}
			continue
		}
		output.WriteString(segment.Text)
	}
	return output.String(), true
}

func keyAt(lines []string, index int) lineKey {
	return lineKey{lines[index], lines[index+1], lines[index+2]}
}

func matchingLines(previous, current []string, previousIndex, currentIndex int) int {
	matched := 0
	for previousIndex+matched < len(previous) && currentIndex+matched < len(current) &&
		previous[previousIndex+matched] == current[currentIndex+matched] {
		matched++
	}
	return matched
}
