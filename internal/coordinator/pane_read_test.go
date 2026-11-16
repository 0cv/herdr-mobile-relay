package coordinator

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestHandleReadPaneUsesRecentRowsForResizedPane(t *testing.T) {
	recentLines := make([]string, 120)
	for index := range recentLines {
		recentLines[index] = fmt.Sprintf("recent row %03d", index)
	}
	recent := strings.Join(recentLines, "\n") + "\n"
	client := newFakeBackend()
	client.contents["pane-1"] = recent
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: "pane-1", Agent: "omp", Status: "idle"}}, state.RevisionCounter())
	dispatcher := newTestDispatcher(t, client, state)

	response := dispatcher.HandleReadPane(context.Background(), PaneReadRequest{
		PaneID: "pane-1", Lines: 150, Format: "ansi", Columns: 20, Rows: 46,
	})
	if response["content"] != recent {
		t.Fatalf("pane content has %d lines, want the %d-line recent window",
			strings.Count(response["content"].(string), "\n"),
			len(recentLines),
		)
	}
	if response["viewport_only"] != true {
		t.Fatalf("viewport_only = %#v, want true", response["viewport_only"])
	}
	if response["viewport_rows"] != 46 {
		t.Fatalf("viewport_rows = %#v, want 46", response["viewport_rows"])
	}
	if response["truncated"] != false {
		t.Fatalf("pane truncation = %#v, want false", response["truncated"])
	}
	want := []fakeCall{{Method: "ReadPaneRecent", Args: []any{"pane-1", 150, "ansi"}}}
	if got := client.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pane read did not request the recent resized window: %+v", got)
	}
}

// Display reads serve physical rows so the Resize Session baseline shares row
// semantics with resized frames; only Claude keeps logical lines for its
// alternate-screen history merge. Both apply to ansi reads, the only format
// the app asks for.
func TestHandleReadPaneSourceFollowsAgent(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-omp"] = "content"
	client.contents["pane-claude"] = "content"
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-omp", Agent: "omp", Status: "idle"},
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := newTestDispatcher(t, client, state)

	for _, paneID := range []string{"pane-omp", "pane-claude"} {
		response := dispatcher.HandleReadPane(context.Background(), PaneReadRequest{
			PaneID: paneID, Lines: 100, Format: "ansi",
		})
		if _, failed := response["error"]; failed {
			t.Fatalf("read %s failed: %#v", paneID, response)
		}
	}
	want := []fakeCall{
		{Method: "ReadPaneRecent", Args: []any{"pane-omp", 100, "ansi"}},
		{Method: "ReadPane", Args: []any{"pane-claude", 100, "ansi"}},
	}
	if got := client.recorded(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reads = %+v, want %+v", got, want)
	}
}

// A "recent"/"recent-unwrapped" read in text format above the pane's viewport
// height makes Herdr harvest scrollback through the agent's mouse-scroll
// interface, which visibly scrolls the operator's real pane. No display read
// may reach that path.
func TestTextFormatDisplayReadNeverHarvestsScrollback(t *testing.T) {
	client := newFakeBackend()
	client.contents["pane-omp"] = "content"
	client.contents["pane-claude"] = "content"
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{
		{PaneID: "pane-omp", Agent: "omp", Status: "idle"},
		{PaneID: "pane-claude", Agent: "claude", Status: "idle"},
	}, state.RevisionCounter())
	dispatcher := newTestDispatcher(t, client, state)

	for _, paneID := range []string{"pane-omp", "pane-claude"} {
		for _, columns := range []int{0, 59} {
			response := dispatcher.HandleReadPane(context.Background(), PaneReadRequest{
				PaneID: paneID, Lines: 400, Format: "text", Columns: columns,
			})
			if _, failed := response["error"]; failed {
				t.Fatalf("read %s failed: %#v", paneID, response)
			}
		}
	}
	if client.count("ReadPaneVisible") != 4 || len(client.recorded()) != 4 {
		t.Fatalf("text-format reads did not all use the visible screen: %+v", client.recorded())
	}
}
