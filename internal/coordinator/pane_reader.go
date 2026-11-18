package coordinator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type paneRead struct {
	done chan struct{}
	read herdr.PaneRead
	err  error
}

type paneReader struct {
	herdr backend.Backend
	state *State
	mu    sync.Mutex
	reads map[string]*paneRead
}

func capPaneContentLines(content []byte, limit int) []byte {
	if limit < 1 || len(content) == 0 {
		return content
	}
	end := len(content)
	if content[end-1] == '\n' {
		end--
	}
	remaining := limit
	for index := end - 1; index >= 0; index-- {
		if content[index] != '\n' {
			continue
		}
		remaining--
		if remaining == 0 {
			return content[index+1:]
		}
	}
	return content
}

// readForDisplay returns physical rows ("recent") for every ansi display read:
// the visible screen alone loses rows that scroll past between watch polls,
// and the Resize Session baseline must share row semantics with the resized
// frames so the trailing desktop screen — including agent chrome — can be cut
// by exact row count. Claude keeps logical lines ("recent-unwrapped") because
// its alternate-screen history merge depends on them.
func (r *paneReader) readForDisplay(
	ctx context.Context,
	paneID string,
	lines int,
	format string,
	resized bool,
) (herdr.PaneRead, error) {
	// A non-ansi read is the only shape Herdr can serve by harvesting
	// scrollback through the agent's mouse-scroll interface: asking "recent"
	// or "recent-unwrapped" in text format for more lines than the pane's
	// viewport holds makes Herdr scroll the operator's real pane up and snap
	// it back, once per read. Only "visible" cannot trigger the harvest.
	if format != "ansi" {
		return r.herdr.ReadPaneVisible(ctx, paneID, lines, format)
	}
	if !resized {
		if agent, ok := r.state.Agent(paneID); ok && agentkind.Of(agent.Agent) == agentkind.Claude {
			return r.herdr.ReadPane(ctx, paneID, lines, format)
		}
	}
	return r.herdr.ReadPaneRecent(ctx, paneID, lines, format)
}

type PaneReadRequest struct {
	PaneID    string
	RequestID string
	Format    string
	Lines     int
	Columns   int
	Rows      int
}

func (r *paneReader) read(ctx context.Context, request PaneReadRequest) map[string]any {
	paneID := request.PaneID
	if paneID == "" {
		return map[string]any{"type": "pane_content", "pane_id": "", "content": "", "format": "text"}
	}
	lines := request.Lines
	if lines < 1 {
		lines = 1
	}
	if lines > 10000 {
		lines = 10000
	}
	format := request.Format
	if format != "ansi" {
		format = "text"
	}
	terminalColumns := request.Columns
	if terminalColumns < 1 {
		terminalColumns = 0
	}
	readCtx, cancel := context.WithTimeout(ctx, commandDeadline)
	defer cancel()
	generation := r.state.Generation(paneID)
	revision := r.state.ContentRevision(paneID)
	key := fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d\x00%s", paneID, lines, format, terminalColumns, generation, request.RequestID)
	r.mu.Lock()
	call := r.reads[key]
	owner := call == nil
	if owner {
		call = &paneRead{done: make(chan struct{})}
		r.reads[key] = call
	}
	r.mu.Unlock()
	if owner {
		call.read, call.err = r.readForDisplay(readCtx, paneID, lines, format, terminalColumns > 0)
		r.mu.Lock()
		delete(r.reads, key)
		close(call.done)
		r.mu.Unlock()
	} else {
		select {
		case <-call.done:
		case <-readCtx.Done():
			return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "Unable to read the agent pane"}
		}
	}
	read, err := call.read, call.err
	if err != nil {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "Unable to read the agent pane"}
	}
	if r.state.Generation(paneID) != generation {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "The agent pane was replaced while it was being read"}
	}
	if r.state.ContentRevision(paneID) != revision {
		return map[string]any{"type": "pane_content", "pane_id": paneID, "content": "", "format": format, "error": "The agent state changed while the pane was being read"}
	}
	content := capPaneContentLines(read.Content, lines)
	response := map[string]any{
		"type": "pane_content", "pane_id": paneID, "content": string(content),
		"format": format, "truncated": read.Truncated, "viewport_only": terminalColumns > 0,
		"interaction": nil,
	}
	if terminalRows := request.Rows; terminalRows > 0 {
		response["viewport_rows"] = terminalRows
	}
	return response
}

func (r *paneReader) probe(ctx context.Context, request PaneReadRequest) map[string]any {
	paneID := request.PaneID
	format := request.Format
	if format != "ansi" {
		format = "text"
	}
	if paneID == "" {
		return map[string]any{"type": "pane_probe", "pane_id": "", "format": format, "error": "Unable to read the agent pane"}
	}
	const lines = 500
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	generation := r.state.Generation(paneID)
	contentRevision := r.state.ContentRevision(paneID)
	content, err := r.herdr.ProbePaneVisible(readCtx, paneID, lines, format)
	if err != nil {
		return map[string]any{"type": "pane_probe", "pane_id": paneID, "format": format, "error": "Unable to read the agent pane"}
	}
	if r.state.Generation(paneID) != generation || r.state.ContentRevision(paneID) != contentRevision {
		return map[string]any{"type": "pane_probe", "pane_id": paneID, "format": format, "error": "The agent pane changed while it was being read"}
	}
	return map[string]any{"type": "pane_probe", "pane_id": paneID, "content": string(content), "format": format}
}
