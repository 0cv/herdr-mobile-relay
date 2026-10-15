package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
)

func p1WaitReloadReady(ctx context.Context, read func(context.Context) (herdr.PaneRead, error)) (herdr.PaneRead, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return herdr.PaneRead{}, err
		}
		screen, err := read(ctx)
		if err != nil {
			return screen, err
		}
		if bytes.Contains(screen.Content, []byte("Reloaded keybindings, extensions, skills, prompts, themes, and context files")) && !bytes.Contains(screen.Content, []byte("Reloading keybindings")) {
			return screen, nil
		}
		select {
		case <-ctx.Done():
			return screen, fmt.Errorf("Pi reload did not restore input readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestDisposablePiSettingsAndReload(t *testing.T) {
	if os.Getenv("HERDR_SCROLL_FIXTURE_MODE") != "regular" {
		t.Skip("requires the second owned Pi incarnation")
	}
	contract, err := p1fixture.Current()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := herdr.NewClient(os.Getenv("HERDR_SCROLL_FIXTURE_BIN"), contract.SocketPath)
	defer client.Close()
	observer, err := herdr.NewScrollEvidence(512*1024, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetScrollEvidence(observer); err != nil {
		t.Fatal(err)
	}
	defer func() {
		records, err := observer.Snapshot()
		if err != nil {
			t.Error(err)
			return
		}
		if err := contract.Save("lifecycle-wire.json", map[string]any{"failed": t.Failed(), "records": records}); err != nil {
			t.Error(err)
		}
	}()
	client.RefreshCapabilities(ctx)
	pane := os.Getenv("HERDR_SCROLL_FIXTURE_PANE")
	processes, err := client.SocketPaneProcessInfo(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := foregroundRoot(processes, pane)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := pibridge.Instance(contract.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	identity := pibridge.Identity{Instance: instance, Pane: pane, Session: os.Getenv("HERDR_SCROLL_FIXTURE_SESSION"), PID: pid}
	initial, err := pibridge.QueryScrollState(ctx, identity)
	if err != nil || initial.Renderer != "regular" {
		t.Fatalf("initial real renderer: %+v %v", initial, err)
	}
	send := func(text string) {
		t.Helper()
		if err := client.SendRawText(ctx, pane, text); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	command := func(text string) {
		t.Helper()
		send(text)
		send("\r")
	}
	wait := func(mode, oldIncarnation string) pibridge.ScrollResponse {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		var snapshot pibridge.ScrollResponse
		for time.Now().Before(deadline) {
			var err error
			snapshot, err = pibridge.QueryScrollState(ctx, identity)
			if err == nil && snapshot.Renderer == mode && (oldIncarnation == "" || snapshot.Incarnation != oldIncarnation) {
				return snapshot
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("live mode/incarnation did not change: %+v", snapshot)
		return snapshot
	}
	command("/p1-overlay")
	command("/settings")
	send("TUI mode")
	send("\r")
	blocked := wait("regular", "")
	blockedScreen, err := client.ReadScrollEvidencePane(ctx, pane, "visible", "text", 80)
	if err != nil || !bytes.Contains(blockedScreen.Content, []byte("Close active overlays before changing TUI mode")) {
		t.Fatalf("actual overlay did not block the settings switch: %s %v", blockedScreen.Content, err)
	}
	if blocked.Incarnation != initial.Incarnation || blocked.Revision != initial.Revision {
		t.Fatal("blocked switch changed observed renderer state")
	}
	overlayDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(overlayDeadline) {
		if _, err := os.Stat(filepath.Join(contract.Root, "overlay-done")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(contract.Root, "overlay-done")); err != nil {
		t.Fatal("private overlay did not finish")
	}
	send("\r")
	fullscreen := wait("fullscreen", "")
	if fullscreen.Incarnation != initial.Incarnation || fullscreen.Revision == initial.Revision {
		t.Fatal("renderer replacement lost process incarnation or failed to invalidate state")
	}
	send("\x1b")
	send("\x1b")
	gridRunner := &p1fixture.Runner{}
	grid, err := panesize.NewReader(gridRunner, "").Read(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	widgetBefore, err := client.ReadScrollEvidencePane(ctx, pane, "visible", "text", grid.Rows)
	if err != nil {
		t.Fatal(err)
	}
	command("/p1-remove-widget")
	removed := wait("unknown", "")
	widgetAfter, err := client.ReadScrollEvidencePane(ctx, pane, "visible", "text", grid.Rows)
	if err != nil || !bytes.Equal(widgetBefore.Content, widgetAfter.Content) {
		t.Fatalf("invisible widget removal changed the actual grid: before=%s after=%s error=%v", widgetBefore.Content, widgetAfter.Content, err)
	}
	command("/reload")
	widgetRestored := wait("fullscreen", initial.Incarnation)
	reloadCtx, stopReload := context.WithTimeout(ctx, 2*time.Second)
	defer stopReload()
	reloadScreen, err := p1WaitReloadReady(reloadCtx, func(ctx context.Context) (herdr.PaneRead, error) {
		return client.ReadScrollEvidencePane(ctx, pane, "visible", "text", grid.Rows)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Save("reload-ready.json", reloadScreen.Content); err != nil {
		t.Fatal(err)
	}
	gridCommands, err := gridRunner.Observations()
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Save("widget-baseline.json", map[string]any{"grid": grid, "commands": gridCommands, "before": widgetBefore.Content, "after": widgetAfter.Content, "removed": removed, "restored": widgetRestored}); err != nil {
		t.Fatal(err)
	}
	send("\x07")
	editorDeadline := time.Now().Add(time.Second)
	for time.Now().Before(editorDeadline) {
		if _, err := os.Stat(filepath.Join(contract.Root, "editor-active")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(contract.Root, "editor-active")); err != nil {
		t.Fatal("private external editor did not start")
	}
	editorGroup, err := client.SocketPaneProcessInfo(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foregroundRoot(editorGroup, pane); err == nil || len(editorGroup.ForegroundProcesses) < 2 {
		t.Fatalf("actual inherited-stdio editor group was admitted: %+v", editorGroup)
	}
	state := coordinator.NewState(slog.Default())
	state.CommitInventory([]*coordinator.AgentState{{PaneID: pane, TerminalID: os.Getenv("HERDR_SCROLL_FIXTURE_TERMINAL"), SessionID: identity.Session, Agent: "pi", Status: "idle"}}, 0)
	dispatcher := coordinator.NewDispatcherWithPiScroll(client, state, nil, slog.Default(), contract.SocketPath, &p1fixture.Runner{})
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := dispatcher.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	unavailable := dispatcher.ScrollState(ctx, pane, "editor-fixture", true)
	if unavailable == nil || unavailable.Available {
		t.Fatalf("actual editor did not invalidate ownership: %+v", unavailable)
	}
	refusal := dispatcher.Handle(ctx, map[string]any{"action": "scroll_agent", "pane_id": pane, "request_id": "editor-refusal", "_scroll_owner": "editor-fixture", "state_revision": unavailable.Revision, "direction": "up"})
	if refusal.OK || refusal.Phase != "not_started" {
		t.Fatalf("editor accepted wheel: %+v", refusal)
	}
	if err := contract.Save("editor-handoff.json", map[string]any{"group": editorGroup, "state": unavailable, "result": refusal}); err != nil {
		t.Fatal(err)
	}
	editorDeadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(editorDeadline) {
		if _, err := os.Stat(filepath.Join(contract.Root, "editor-done")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(contract.Root, "editor-done")); err != nil {
		t.Fatal("private editor did not exit")
	}
	wait("fullscreen", "")
	command("/settings")
	send("TUI mode")
	send("\r")
	regular := wait("regular", "")
	send("\x1b")
	send("\x1b")
	command("/reload")
	reloaded := wait("regular", widgetRestored.Incarnation)
	records, err := observer.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Save("settings-reload.json", map[string]any{"initial": initial, "blocked": blocked, "blocked_screen": blockedScreen.Content, "fullscreen": fullscreen, "regular": regular, "reloaded": reloaded, "records": records}); err != nil {
		t.Fatal(err)
	}
}
