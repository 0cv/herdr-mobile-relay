package herdr

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPaneProcessInfoDecodesForegroundProcess(t *testing.T) {
	bin := writeResultScript(t, `{"result":{"process_info":{"pane_id":"pane-1","shell_pid":101,"foreground_process_group_id":202,"foreground_processes":[{"pid":202,"name":"codex","cwd":"/work","cmdline":"codex --resume","argv":["codex","--resume"]}]}}}`)
	client := NewClient(bin, filepath.Join(t.TempDir(), "herdr.sock"))

	info, err := client.PaneProcessInfo(context.Background(), "pane-1")
	if err != nil {
		t.Fatalf("PaneProcessInfo() error = %v", err)
	}
	if info.PaneID != "pane-1" || info.ShellPID != 101 || info.ForegroundProcessGroupID != 202 {
		t.Fatalf("PaneProcessInfo() = %#v", info)
	}
	if len(info.ForegroundProcesses) != 1 {
		t.Fatalf("foreground processes = %d, want 1", len(info.ForegroundProcesses))
	}
	process := info.ForegroundProcesses[0]
	if process.PID != 202 || process.Name != "codex" || process.Cwd != "/work" || len(process.Argv) != 2 {
		t.Fatalf("foreground process = %#v", process)
	}
}
