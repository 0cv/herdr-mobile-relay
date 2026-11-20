package conversation

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
)

func (page BrowsePage) WithEditDiffs(agent string) BrowsePage {
	AddEditDiffs(agent, page, browseResponseBytes, &page.Provisional, &page.Entries)
	return page
}

// AddEditDiffs replaces reply slices only where a preview fits. Lists are ordered newest first.
func AddEditDiffs(agent string, base any, limit int, lists ...*[]Entry) {
	encoded, err := json.Marshal(base)
	if err != nil {
		return
	}
	remainingBytes, remainingCells := limit-len(encoded), 2_000_000
	agent = agentkind.Of(agent)
	for _, list := range lists {
		entries := *list
		copiedEntries := false
		for i := len(entries) - 1; i >= 0; i-- {
			if remainingBytes < minEditDiffFieldBytes {
				return
			}
			tools := entries[i].Tools
			copiedTools := false
			for j := len(tools) - 1; j >= 0; j-- {
				if remainingBytes < minEditDiffFieldBytes {
					break
				}
				diff := tools[j].editDiff(agent, &remainingCells)
				if diff == nil {
					continue
				}
				size, ok := fitEditDiff(diff, remainingBytes)
				if !ok {
					continue
				}
				if !copiedEntries {
					entries = slices.Clone(entries)
					*list = entries
					copiedEntries = true
				}
				if !copiedTools {
					tools = slices.Clone(tools)
					entries[i].Tools = tools
					copiedTools = true
				}
				tools[j].Diff = diff
				remainingBytes -= size
			}
		}
	}
}

const editDiffFieldPrefix = `,"diff":`
const minEditDiffFieldBytes = len(editDiffFieldPrefix + `{"kind":"edit","path":"a","added":0,"removed":1,"text":"-","truncated":true}`)

func fitEditDiff(diff *EditDiff, budget int) (int, bool) {
	size := func() int {
		data, _ := json.Marshal(diff)
		return len(editDiffFieldPrefix) + len(data)
	}
	if n := size(); n <= budget {
		return n, true
	}
	text := diff.Text
	diff.Truncated = true
	n := sort.Search(len(text), func(n int) bool {
		diff.Text = utf8Prefix(text, n)
		return size() > budget
	}) - 1
	if n <= 0 {
		return 0, false
	}
	prefix := utf8Prefix(text, n)
	if text[len(prefix)] != '\n' {
		if end := strings.LastIndexByte(prefix, '\n'); end >= 0 {
			prefix = prefix[:end]
		}
	}
	diff.Text = strings.TrimSuffix(prefix, "\n@@")
	return size(), true
}
