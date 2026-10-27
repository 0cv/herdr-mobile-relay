package app

import (
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

func TestRecordedWindowRemovalAndPlanReset(t *testing.T) {
	page := func(ids ...string) conversation.BrowsePage {
		entries := make([]conversation.Entry, len(ids))
		for index, id := range ids {
			entries[index].ID = id
		}
		return conversation.BrowsePage{Entries: entries}
	}
	previous := page("a", "b", "c")
	for _, tc := range []struct {
		name string
		page conversation.BrowsePage
		want bool
	}{
		{"append", page("a", "b", "c", "d"), false},
		{"tail eviction", page("b", "c", "d"), false},
		{"middle deletion", page("a", "c"), true},
		{"last deletion", page("a", "b"), true},
		{"empty", page(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordedWindowRemoved(previous, tc.page); got != tc.want {
				t.Fatalf("removed=%v want=%v", got, tc.want)
			}
		})
	}
	oldTotal, newTotal := 3, 2
	previous.Total = &oldTotal
	current := page("b", "c")
	current.Total = &newTotal
	if !recordedWindowRemoved(previous, current) {
		t.Fatal("oldest deletion did not reset")
	}
	previous.OMOPlan = &conversation.OMOTodoState{Available: true, Version: 2}
	current.OMOPlan = &conversation.OMOTodoState{Available: true, Version: 2}
	if recordedPlanChanged(previous, current) {
		t.Fatal("unchanged plan reset")
	}
	current.OMOPlan.UpdatedAt = "changed"
	if !recordedPlanChanged(previous, current) {
		t.Fatal("changed plan did not reset")
	}
}
