package agentsession

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc"
)

const maxPayload = 1 << 20

type payload map[string]any

func session(agent string, data []byte, getenv func(string) string) string {
	var p payload
	if json.Unmarshal(data, &p) != nil {
		return ""
	}
	id, _ := p["session_id"].(string)
	event, _ := p["hook_event_name"].(string)
	switch agent {
	case "claude":
		_, cursor := p["cursor_version"]
		if getenv("CURSOR_VERSION") != "" || cursor || event != "SessionStart" || truthy(p["agent_id"]) {
			return ""
		}
	case "qoder":
		if _, subagent := p["agent_id"]; subagent || event != "SessionStart" {
			return ""
		}
	case "cursor":
		first := func(names ...string) string {
			for _, name := range names {
				if value, ok := p[name].(string); ok && value != "" {
					return value
				}
			}
			return ""
		}
		event = first("hook_event_name", "hookEventName")
		if event != "" && event != "sessionStart" {
			return ""
		}
		id = first("session_id", "sessionId", "conversation_id", "conversationId")
	case "omp":
	case "codex":
		transcript, _ := p["transcript_path"].(string)
		inherited := getenv("CODEX_THREAD_ID")
		if event != "" && event != "SessionStart" || strings.TrimSpace(transcript) == "" || inherited != "" && inherited != id {
			return ""
		}
	default:
		return ""
	}
	return id
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) != 0
	case map[string]any:
		return len(v) != 0
	}
	return true
}

func Run(agent string, input io.Reader) {
	terminal, socket := os.Getenv("HC_TERMINAL_ID"), os.Getenv("HC_TERMINAL_SOCKET")
	if terminal == "" || socket == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		data, err := io.ReadAll(io.LimitReader(input, maxPayload+1))
		if err != nil || len(data) > maxPayload {
			return
		}
		id := session(agent, data, os.Getenv)
		if id == "" || ctx.Err() != nil {
			return
		}
		client := hc.New(socket)
		info, err := client.PaneProcessInfoFresh(ctx, terminal)
		if err != nil {
			return
		}
		pid := info.ForegroundProcessGroupID
		if !ancestor(ctx, pid, os.Getpid(), parentPID) {
			return
		}
		_ = client.ReportSession(ctx, terminal, id, pid)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func ancestor(ctx context.Context, target, child int, parent func(context.Context, int) (int, error)) bool {
	if target <= 1 {
		return false
	}
	for range 64 {
		if ctx.Err() != nil || child <= 1 {
			return false
		}
		next, err := parent(ctx, child)
		if err != nil || next <= 0 || next == child {
			return false
		}
		if next == target {
			return true
		}
		child = next
	}
	return false
}

func parentPID(ctx context.Context, pid int) (int, error) {
	output, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(output)))
}
