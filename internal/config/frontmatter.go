package config

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

type Frontmatter map[string]string

func ParseFrontmatter(r io.Reader) (Frontmatter, error) {
	fm := make(Frontmatter)
	scanner := bufio.NewScanner(r)
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			continue
		}
		if trimmed[0] == '#' {
			continue
		}

		idx := strings.IndexByte(trimmed, ':')
		if idx < 0 {
			return nil, fmt.Errorf("line %d: no colon delimiter", lineNo)
		}

		key := strings.TrimSpace(trimmed[:idx])
		value := strings.TrimSpace(trimmed[idx+1:])
		fm[key] = value
	}

	return fm, scanner.Err()
}
