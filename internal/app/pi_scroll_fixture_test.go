package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
)

func runFixtureCommand(ctx context.Context, name, input string, args ...string) ([]byte, error) {
	return p1fixture.Run(ctx, name, input, args...)
}

type fixturePaneCommandRunner struct{}

func (fixturePaneCommandRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "ps" && name != "stty" {
		return nil, errors.New("fixture pane helper is unsupported")
	}
	return runFixtureCommand(ctx, name, "", args...)
}

func piScrollFixtureDependencies() serverDependencies {
	return serverDependencies{
		geometryRunner:  fixturePaneCommandRunner{},
		speechLanguages: func() []string { return nil },
	}
}

type measuredGridRunner struct {
	calls        map[string]int
	times        map[string]time.Duration
	observations []p1fixture.CommandObservation
}

func (r *measuredGridRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	start := time.Now()
	r.calls[name]++
	output, err := (fixturePaneCommandRunner{}).Output(ctx, name, args...)
	r.times[name] += time.Since(start)
	record := p1fixture.CommandObservation{Name: name, Args: append([]string(nil), args...), Output: append([]byte(nil), output...), Started: start, Elapsed: time.Since(start)}
	if err != nil {
		record.Error = err.Error()
	}
	r.observations = append(r.observations, record)
	return output, err
}

func verifyDisposablePiFixture(t *testing.T, socket string) {
	t.Helper()
	contract, err := p1fixture.Current()
	if err != nil || contract.SocketPath != socket {
		t.Fatalf("owned fixture contract refused: %v", err)
	}

}

func fixtureHistoryMarkers(data []byte) ([]int, error) {
	matches := regexp.MustCompile(`P1 TEST USER \[([0-9]{3})\]`).FindAllSubmatch(data, -1)
	markers := make([]int, 0, len(matches))
	for _, match := range matches {
		value, err := strconv.Atoi(string(match[1]))
		if err != nil || value < 1 || value > 96 || len(markers) > 0 && value <= markers[len(markers)-1] {
			return nil, errors.New("native history markers are ambiguous or unordered")
		}
		markers = append(markers, value)
	}
	if len(markers) == 0 {
		return nil, errors.New("native history contains no generated user markers")
	}
	return markers, nil
}

func captureFixtureRegularHistory(ctx context.Context, contract *p1fixture.Contract, client *herdr.Client, observer *herdr.ScrollEvidence, pane string, grid panesize.Grid) error {
	recent, err := client.ReadScrollEvidencePane(ctx, pane, "recent", "text", 4096)
	if err != nil {
		return err
	}
	visible, err := client.ReadScrollEvidencePane(ctx, pane, "visible", "text", grid.Rows)
	if err != nil {
		return err
	}
	historyMarkers, err := fixtureHistoryMarkers(recent.Content)
	if err != nil {
		return err
	}
	viewportMarkers, err := fixtureHistoryMarkers(visible.Content)
	if err != nil {
		return err
	}
	olderMarkers := []int{}
	for _, marker := range historyMarkers {
		if marker < viewportMarkers[0] {
			olderMarkers = append(olderMarkers, marker)
		}
	}
	rowsBeyondViewport := len(strings.Split(strings.TrimSuffix(string(recent.Content), "\n"), "\n")) - grid.Rows
	if len(olderMarkers) == 0 || rowsBeyondViewport <= 0 {
		return errors.New("regular Pi rendering did not produce actual older Herdr rows beyond the measured viewport")
	}
	records, err := observer.Snapshot()
	if err != nil {
		return err
	}
	reads := []herdr.ScrollEvidenceRecord{}
	for _, record := range records {
		if record.Method == "pane.read" {
			reads = append(reads, record)
		}
	}
	if len(reads) != 2 {
		return errors.New("native history read provenance is not unique")
	}
	if err := contract.Save("herdr-history-wire.json", map[string]any{"schema_version": 1, "kind": "p1-unary-observations", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "records": reads}); err != nil {
		return err
	}
	return contract.Save("herdr-history-1.json", map[string]any{"schema_version": 1, "kind": "p1-herdr-history", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "mode": "regular", "wireRecordFile": "herdr-history-wire.json", "olderMarkers": olderMarkers, "viewportMarkers": viewportMarkers, "rowsBeyondViewport": rowsBeyondViewport, "measuredGrid": grid})
}

func captureFixtureBridgeWithhold(ctx context.Context, contract *p1fixture.Contract, client *herdr.Client, observer *herdr.ScrollEvidence, terminal string, identity pibridge.Identity) (resultErr error) {
	if terminal == "" {
		return errors.New("missing exact native terminal for bridge refusal control")
	}
	state := coordinator.NewState(slog.Default())
	state.CommitInventory([]*coordinator.AgentState{{PaneID: identity.Pane, TerminalID: terminal, SessionID: identity.Session, Agent: "pi", Status: "idle"}}, 0)
	dispatcher := coordinator.NewDispatcherWithPiScroll(client, state, nil, slog.Default(), contract.SocketPath, &p1fixture.Runner{})
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		resultErr = errors.Join(resultErr, dispatcher.Close(closeCtx))
	}()
	directory, err := filepath.EvalSymlinks(pibridge.Directory(identity.Instance))
	if err != nil {
		return err
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0700 || int(directoryStat.Uid) != os.Getuid() {
		return errors.New("bridge control directory is not privately owned")
	}
	socketPath := filepath.Join(directory, fmt.Sprintf("%d.sock", identity.PID))
	holdingPath := socketPath + ".p1-withheld"
	endpoint := func(path string, links uint64) (os.FileInfo, map[string]any, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || int(stat.Uid) != os.Getuid() || uint64(stat.Nlink) != links {
			return nil, nil, errors.New("bridge control endpoint is not an owned socket")
		}
		return info, map[string]any{"dev": stat.Dev, "ino": stat.Ino, "uid": stat.Uid, "mode": info.Mode().Perm(), "ownerPid": identity.PID}, nil
	}
	before, beforeRecord, err := endpoint(socketPath, 1)
	if err != nil {
		return err
	}
	beforeState, err := pibridge.QueryScrollState(ctx, identity)
	if err != nil {
		return err
	}
	confirmed, _, err := endpoint(socketPath, 1)
	if err != nil || !os.SameFile(before, confirmed) {
		return errors.New("bridge endpoint changed after peer-checked query")
	}
	if _, err := os.Lstat(holdingPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("bridge holding path is not absent")
	}
	started := time.Now()
	if err := os.Link(socketPath, holdingPath); err != nil {
		return err
	}
	held, _, heldErr := endpoint(holdingPath, 2)
	linked, _, linkedErr := endpoint(socketPath, 2)
	currentDirectory, directoryErr := os.Lstat(directory)
	if heldErr != nil || linkedErr != nil || !os.SameFile(before, held) || !os.SameFile(before, linked) || directoryErr != nil || !os.SameFile(directoryInfo, currentDirectory) {
		return errors.New("bridge withhold identity changed; retain both endpoint paths")
	}
	if err := os.Remove(socketPath); err != nil {
		return err
	}
	held, _, heldErr = endpoint(holdingPath, 1)
	_, absentErr := os.Lstat(socketPath)
	if heldErr != nil || !os.SameFile(before, held) || !errors.Is(absentErr, os.ErrNotExist) {
		return errors.New("bridge absence identity changed; retain both endpoint paths")
	}
	_, refusal := pibridge.QueryScrollState(ctx, identity)
	captureRefusal := func() (map[string]any, error) {
		probe, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		windowStart := time.Now()
		beforeWire, err := observer.Snapshot()
		if err != nil {
			return nil, err
		}
		select {
		case <-probe.Done():
			return nil, probe.Err()
		case <-time.After(25 * time.Millisecond):
		}
		unknown := dispatcher.ScrollState(probe, identity.Pane, "bridge-withhold", true)
		if unknown == nil || unknown.Renderer != "unknown" || unknown.Available || unknown.Revision == "" {
			return nil, errors.New("withheld native bridge did not derive an unknown dispatcher state")
		}
		result := dispatcher.Handle(probe, map[string]any{"action": "scroll_agent", "request_id": "bridge-withhold-native-1", "pane_id": identity.Pane, "_scroll_owner": "bridge-withhold", "state_revision": unknown.Revision, "direction": "up"})
		if result == nil || result.OK || result.Error == "" || result.Phase != "not_started" {
			return nil, errors.New("unknown native bridge did not refuse the wheel intent before dispatch")
		}
		select {
		case <-probe.Done():
			return nil, probe.Err()
		case <-time.After(25 * time.Millisecond):
		}
		afterWire, err := observer.Snapshot()
		if err != nil {
			return nil, err
		}
		if len(afterWire) <= len(beforeWire) {
			return nil, errors.New("native bridge refusal lacks actual resolver unary observations")
		}
		records := afterWire[len(beforeWire):]
		for _, record := range records {
			if record.Method == "pane.send_text" || record.Method == "pane.send_keys" || record.Overflow || record.Error != "" {
				return nil, errors.New("native bridge refusal window contains a send or incomplete unary observation")
			}
		}
		return map[string]any{"started": windowStart, "ended": time.Now(), "state": unknown, "result": result, "records": records, "quietBeforeMS": 25, "quietAfterMS": 25}, nil
	}
	refusalWindow, captureErr := captureRefusal()
	held, _, heldErr = endpoint(holdingPath, 1)
	_, absentErr = os.Lstat(socketPath)
	currentDirectory, directoryErr = os.Lstat(directory)
	if heldErr != nil || !os.SameFile(before, held) || !errors.Is(absentErr, os.ErrNotExist) || directoryErr != nil || !os.SameFile(directoryInfo, currentDirectory) {
		return errors.New("bridge restore identity changed; retain both endpoint paths")
	}
	if err := os.Link(holdingPath, socketPath); err != nil {
		return err
	}
	restored, restoredRecord, err := endpoint(socketPath, 2)
	held, _, heldErr = endpoint(holdingPath, 2)
	if err != nil || heldErr != nil || !os.SameFile(before, restored) || !os.SameFile(before, held) {
		return errors.New("restored bridge endpoint changed; retain both endpoint paths")
	}
	if err := os.Remove(holdingPath); err != nil {
		return err
	}
	restored, restoredRecord, err = endpoint(socketPath, 1)
	if err != nil || !os.SameFile(before, restored) {
		return errors.New("restored bridge endpoint does not match the original identity")
	}
	delete(restoredRecord, "ownerPid")
	if refusal == nil {
		return errors.New("withheld bridge was unexpectedly available")
	}
	afterState, err := pibridge.QueryScrollState(ctx, identity)
	if err != nil {
		return err
	}
	if beforeState.Identity != afterState.Identity || beforeState.Incarnation != afterState.Incarnation || beforeState.Revision != afterState.Revision || beforeState.Renderer != afterState.Renderer || beforeState.Wheel != afterState.Wheel {
		return errors.New("bridge state changed across withhold and restoration")
	}
	captureError := ""
	if captureErr != nil {
		captureError = captureErr.Error()
	}
	saveErr := contract.Save("bridge-withhold-1.json", map[string]any{"schema_version": 1, "kind": "p1-bridge-withhold", "source_kind": "real", "complete": captureErr == nil, "truncated": false, "ambiguous": false, "skipped": false, "socketPath": socketPath, "holdingPath": holdingPath, "before": beforeRecord, "during": map[string]any{"originalAbsent": true, "queryError": refusal.Error()}, "restored": restoredRecord, "result": "unavailable-then-restored", "started": started, "ended": time.Now(), "beforeState": beforeState, "afterState": afterState, "refusalWindow": refusalWindow, "captureError": captureError, "sameUidInterferenceProvedAbsent": false})
	return errors.Join(captureErr, saveErr)
}

func TestDisposablePiScrollFoundation(t *testing.T) {
	socket, pane, session := os.Getenv("HERDR_SCROLL_FIXTURE_SOCKET"), os.Getenv("HERDR_SCROLL_FIXTURE_PANE"), os.Getenv("HERDR_SCROLL_FIXTURE_SESSION")
	if socket == "" || pane == "" || session == "" {
		t.Skip("requires an isolated Pi scroll fixture")
	}
	verifyDisposablePiFixture(t, socket)
	contract, err := p1fixture.Current()
	if err != nil {
		t.Fatal(err)
	}
	client := herdr.NewClient(os.Getenv("HERDR_SCROLL_FIXTURE_BIN"), socket)
	defer client.Close()
	observer, err := herdr.NewScrollEvidence(1024*1024, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetScrollEvidence(observer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status := client.RefreshCapabilities(ctx)
	payload, _ := json.Marshal(status)
	t.Logf("fixture_capabilities=%s", payload)
	if !status.ServerProtocolKnown || status.ServerVersion == "" || status.InstalledClientVersion == "" {
		t.Fatal("missing client/server capability evidence")
	}
	instance, err := pibridge.Instance(socket)
	if err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("HERDR_SCROLL_FIXTURE_MODE")
	if mode != "regular" && mode != "fullscreen" {
		t.Fatal("missing exact native renderer selection")
	}
	var lastGrid panesize.Grid
	for tick := range 6 {
		start := time.Now()
		admission := start.Add(250 * time.Millisecond)
		flight, stop := context.WithDeadline(ctx, admission)
		phase := map[string]time.Duration{}
		runner := &measuredGridRunner{calls: map[string]int{}, times: map[string]time.Duration{}}
		readStart := time.Now()
		before, err := client.SocketPaneProcessInfo(flight, pane)
		phase["process_before"] = time.Since(readStart)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		pid, err := foregroundRoot(before, pane)
		if err != nil {
			stop()
			t.Fatalf("fixture ownership: %v, %+v", err, before)
		}
		readStart = time.Now()
		evidence, err := pibridge.QueryScrollState(flight, pibridge.Identity{Instance: instance, Pane: pane, Session: session, PID: pid})
		phase["bridge"] = time.Since(readStart)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		readStart = time.Now()
		grid, err := panesize.NewReader(runner, "").Read(flight, pid)
		phase["geometry"] = time.Since(readStart)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		readStart = time.Now()
		after, err := client.SocketPaneProcessInfo(flight, pane)
		phase["process_after"] = time.Since(readStart)
		stop()
		if err != nil {
			t.Fatalf("preflight failed after %v: %v; phases=%v", time.Since(start), err, phase)
		}
		current, err := foregroundRoot(after, pane)
		if err != nil || current != pid {
			t.Fatal("foreground process replaced")
		}
		if time.Now().After(admission) {
			t.Fatalf("preflight exceeded 250ms: %v", phase)
		}
		if evidence.Renderer != mode {
			t.Fatalf("renderer = %q, want %q", evidence.Renderer, mode)
		}
		lastGrid = grid
		if runner.calls["ps"] != 1 || runner.calls["stty"] != 1 {
			t.Fatalf("unexpected geometry cost: %v", runner.calls)
		}
		if mode == "fullscreen" {
			ack, done := context.WithDeadline(ctx, start.Add(500*time.Millisecond))
			ack = herdr.WithDispatchCheck(ack, func() error {
				if !time.Now().Before(admission) {
					return context.DeadlineExceeded
				}
				return nil
			})
			readStart = time.Now()
			err = client.SendRawText(ack, pane, fmt.Sprintf("\x1b[<64;%d;%dM", max(1, (grid.Columns+1)/2), max(1, (grid.Rows+1)/2)))
			phase["raw_write_ack"] = time.Since(readStart)
			done()
			if err != nil {
				t.Fatal(err)
			}
		} else if evidence.Wheel != "unavailable" {
			t.Fatal("regular mode enabled wheel input")
		}
		if time.Now().After(start.Add(500 * time.Millisecond)) {
			t.Fatal("foundation acknowledgement exceeded 500ms")
		}
		if err := contract.Save(fmt.Sprintf("foundation-%02d.json", tick), map[string]any{"schema_version": 1, "kind": "p1-foundation-tick", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "started": start, "grid": grid, "process_before": before, "process_after": after, "bridge": evidence, "phases": phase, "commands": runner.observations, "calls": runner.calls, "elapsed_ns": time.Since(start)}); err != nil {
			t.Fatal(err)
		}
		t.Logf("tick=%d grid=%+v phases=%v subprocesses=%v subprocess_times=%v total=%v", tick, grid, phase, runner.calls, runner.times, time.Since(start))
		time.Sleep(100 * time.Millisecond)
	}
	if mode == "regular" {
		if err := captureFixtureRegularHistory(ctx, contract, client, observer, pane, lastGrid); err != nil {
			t.Fatal(err)
		}
		info, err := client.SocketPaneProcessInfo(ctx, pane)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := foregroundRoot(info, pane)
		if err != nil {
			t.Fatal(err)
		}
		if err := captureFixtureBridgeWithhold(ctx, contract, client, observer, os.Getenv("HERDR_SCROLL_FIXTURE_TERMINAL"), pibridge.Identity{Instance: instance, Pane: pane, Session: session, PID: pid}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := observer.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for index, record := range records {
		if err := contract.Save(fmt.Sprintf("foundation-wire-%03d.json", index), map[string]any{"schema_version": 1, "kind": "p1-unary-observation", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "record": record}); err != nil {
			t.Fatal(err)
		}
	}
}
