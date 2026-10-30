package app

import (
	"bytes"
	"encoding/json"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func recordedWindowRemoved(previous, page conversation.BrowsePage) bool {
	if previous.Total != nil && page.Total != nil && *page.Total < *previous.Total {
		return true
	}
	current := make(map[string]bool, len(page.Entries))
	for _, entry := range page.Entries {
		current[entry.ID] = true
	}
	seen := false
	for _, entry := range previous.Entries {
		if current[entry.ID] {
			seen = true
		} else if seen {
			return true
		}
	}
	return len(previous.Entries) > 0 && len(page.Entries) == 0
}

func recordedPlanChanged(previous, page conversation.BrowsePage) bool {
	old, _ := json.Marshal(previous.OMOPlan)
	current, _ := json.Marshal(page.OMOPlan)
	return !bytes.Equal(old, current)
}
