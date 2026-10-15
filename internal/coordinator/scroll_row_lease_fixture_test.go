package coordinator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type p1Viewport struct {
	SchemaVersion int `json:"schema_version"`
	Kind string `json:"kind"`
	SourceKind string `json:"source_kind"`
	Complete bool `json:"complete"`
	Truncated bool `json:"truncated"`
	Ambiguous bool `json:"ambiguous"`
	Skipped bool `json:"skipped"`
	ANSI []byte `json:"ansi_base64"`
	Text []byte `json:"text_base64"`
	Markers []int `json:"markers"`
	Grid panesize.Grid `json:"grid"`
	State *protocol.ScrollState `json:"state"`
	Observed time.Time `json:"observed"`
}

var p1MarkerPattern = regexp.MustCompile(`P1 TEST USER \[([0-9]{3})\]`)

func p1Markers(data []byte) ([]int, error) {
	matches := p1MarkerPattern.FindAllSubmatch(data, -1)
	markers := []int{}
	for _, match := range matches {
		number, err := strconv.Atoi(string(match[1]))
		if err != nil || number < 1 || number > 96 || len(markers) > 0 && number <= markers[len(markers)-1] {
			return nil, fmt.Errorf("invalid numbered viewport markers")
		}
		markers = append(markers, number)
	}
	if len(markers) == 0 {
		return nil, fmt.Errorf("viewport has no generated user markers")
	}
	return markers, nil
}

func TestDisposablePiRowLeaseScrollIntegration(t *testing.T) {
	socket := os.Getenv("HERDR_ROW_FIXTURE_SOCKET")
	if socket == "" { t.Skip("requires an owned disposable actual Herdr/Pi fixture") }
	verifyDisposablePiFixture(t, socket)
	contract, err := p1fixture.Current()
	if err != nil { t.Fatal(err) }
	pane, terminal, session := os.Getenv("HERDR_ROW_FIXTURE_PANE"), os.Getenv("HERDR_ROW_FIXTURE_TERMINAL"), os.Getenv("HERDR_ROW_FIXTURE_SESSION")
	if pane == "" || terminal == "" || session == "" { t.Fatal("missing actual fixture identity") }
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := herdr.NewClient(os.Getenv("HERDR_ROW_FIXTURE_BIN"), socket)
	defer client.Close()
	observer, err := herdr.NewScrollEvidence(2*1024*1024, 256)
	if err != nil { t.Fatal(err) }
	if err := client.SetScrollEvidence(observer); err != nil { t.Fatal(err) }
	status := client.RefreshCapabilities(ctx)
	if !client.SupportsPiScrollSocket() { t.Fatalf("actual selected schema not qualified: %+v", status) }
	runner := &p1fixture.Runner{}
	state := NewState(testLogger())
	state.CommitInventory([]*AgentState{{PaneID: pane, TerminalID: terminal, SessionID: session, Agent: "pi", Status: "idle"}}, 0)
	d := NewDispatcherWithPiScroll(client, state, nil, slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})), socket, runner)
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := d.Close(closeCtx); err != nil { t.Errorf("dispatcher closure: %v", err) }
	}()
	leases := panesize.NewManagerWithRunner(client, runner, testLogger())
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := leases.Shutdown(closeCtx); err != nil { t.Errorf("lease closure: %v", err) }
	}()
	notifications := 0
	leases.SetOnResize(func(id string) {
		if id != pane { t.Errorf("wrong pane notification: %s", id) }
		notifications++
		d.InvalidateScrollGeometry(id)
	})
	reader := panesize.NewReader(runner, "")
	info, err := client.SocketPaneProcessInfo(ctx, pane)
	if err != nil || len(info.ForegroundProcesses) != 1 { t.Fatalf("exclusive foreground Pi: %+v %v", info, err) }
	pid := info.ForegroundProcesses[0].PID
	measured, err := reader.Read(ctx, pid)
	if err != nil { t.Fatal(err) }
	readyState := func(rows int) *protocol.ScrollState {
		t.Helper()
		target, ok := d.scrollTarget(pane)
		if !ok { t.Fatal("target unavailable") }
		var snapshot *protocol.ScrollState
		last, stable := "", 0
		deadline := time.Now().Add(1500*time.Millisecond)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if err := d.scroll.Validate(ctx, target, true); err != nil { t.Fatal(err) }
			snapshot = d.ScrollState(ctx, pane, "row-fixture", false)
			if snapshot != nil && snapshot.Available && snapshot.Rows == rows {
				if snapshot.Revision == last { stable++ } else { stable = 1 }
				last = snapshot.Revision
				if stable == 2 { return snapshot }
			}
			time.Sleep(50*time.Millisecond)
		}
		t.Fatalf("stable actual geometry unavailable: %+v", snapshot)
		return nil
	}
	initial := readyState(measured.Rows)
	rows := measured.Rows-8
	if rows < panesize.MinRows { t.Fatal("baseline too short") }
	columns, appliedRows, err := leases.Acquire(ctx, "row-fixture", pane, measured.Columns, rows)
	if err != nil { t.Fatal(err) }
	leasedGrid, err := reader.Read(ctx, pid)
	if err != nil { t.Fatal(err) }
	leased := readyState(rows)
	if columns != measured.Columns || appliedRows != rows || leasedGrid.Rows != rows || leasedGrid.Columns != measured.Columns || leased.GeometryEpoch <= initial.GeometryEpoch || leased.Revision == initial.Revision || notifications != 1 { t.Fatal("row lease did not invalidate measured geometry") }
	command := func(id, revision, direction string) map[string]any { return map[string]any{"action": "scroll_agent", "pane_id": pane, "request_id": id, "_scroll_owner": "row-fixture", "state_revision": revision, "direction": direction} }
	writeCount := func() int {
		records, err := observer.Snapshot()
		if err != nil { t.Fatal(err) }
		count := 0
		for _, record := range records { if record.Method == "pane.send_text" && record.Written > 0 { count++ } }
		return count
	}
	staleCount := writeCount()
	stale := d.Handle(ctx, command("row-stale", initial.Revision, "up"))
	if stale.OK || stale.Phase != "not_started" || writeCount() != staleCount { t.Fatalf("old geometry mutation: %+v", stale) }
	capture := func(grid panesize.Grid, snapshot *protocol.ScrollState) p1Viewport {
		t.Helper()
		actual, err := reader.Read(ctx, pid)
		if err != nil || actual != grid { t.Fatalf("unstable grid: %+v %v", actual, err) }
		ansi, err := client.ReadScrollEvidencePane(ctx, pane, "visible", "ansi", grid.Rows)
		if err != nil { t.Fatal(err) }
		text, err := client.ReadScrollEvidencePane(ctx, pane, "visible", "text", grid.Rows)
		if err != nil { t.Fatal(err) }
		markers, err := p1Markers(text.Content)
		if err != nil { t.Fatal(err) }
		current, err := client.SocketPaneProcessInfo(ctx, pane)
		if err != nil || len(current.ForegroundProcesses) != 1 || current.ForegroundProcesses[0].PID != pid { t.Fatal("foreground replaced during capture") }
		return p1Viewport{SchemaVersion: 1, Kind: "p1-viewport-frame", SourceKind: "real", Complete: true, ANSI: ansi.Content, Text: text.Content, Markers: markers, Grid: actual, State: snapshot, Observed: time.Now()}
	}
	move := func(direction, id string, grid panesize.Grid, snapshot *protocol.ScrollState) (p1Viewport, p1Viewport, string) {
		t.Helper()
		initialRecords, err := observer.Snapshot()
		if err != nil { t.Fatal(err) }
		quietBegan := time.Now()
		before := capture(grid, snapshot)
		time.Sleep(25*time.Millisecond)
		stable := capture(grid, snapshot)
		if fmt.Sprint(before.Markers) != fmt.Sprint(stable.Markers) { t.Fatal("transcript was moving before wheel") }
		code := 64
		if direction == "down" { code = 65 }
		expected := fmt.Sprintf("\x1b[<%d;%d;%dM", code, (grid.Columns+1)/2, (grid.Rows+1)/2)
		started := time.Now()
		result := d.Handle(ctx, command(id, snapshot.Revision, direction))
		ended := time.Now()
		if !result.OK { t.Fatalf("actual wheel refused: %+v", result) }
		deadline := ended.Add(1500*time.Millisecond)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			after := capture(grid, snapshot)
			if direction == "up" && after.Markers[0] < before.Markers[0] || direction == "down" && after.Markers[0] > before.Markers[0] {
				time.Sleep(25*time.Millisecond)
				settled := capture(grid, snapshot)
				quietEnded := time.Now()
				if ctx.Err() != nil || fmt.Sprint(after.Markers) != fmt.Sprint(settled.Markers) { t.Fatal("transcript was moving after wheel") }
				records, err := observer.Snapshot()
				if err != nil || len(records) <= len(initialRecords) { t.Fatalf("missing sender observations: %v", err) }
				windowRecords := records[len(initialRecords):]
				var sends []herdr.ScrollEvidenceRecord
				for _, record := range windowRecords {
					if record.Method == "pane.send_text" { sends = append(sends, record) }
				}
				if len(sends) != 1 || sends[0].RequestID == "" || sends[0].WriteCalls != 1 || sends[0].Written != len(sends[0].Request) || !sends[0].Wrote || sends[0].Overflow || sends[0].Error != "" { t.Fatal("quiet window sender write was not unique and complete") }
				beforeFiles := []string{id+"-before.json", id+"-before-quiet.json"}
				afterFiles := []string{id+"-after.json", id+"-after-quiet.json"}
				for index, frame := range []p1Viewport{before, stable, after, settled} {
					name := append(append([]string{}, beforeFiles...), afterFiles...)[index]
					if err := contract.Save(name, frame); err != nil { t.Fatal(err) }
				}
				wireFile := id+"-window-wire.json"
				if err := contract.Save(wireFile, map[string]any{"schema_version": 1, "kind": "p1-unary-observations", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "records": windowRecords}); err != nil { t.Fatal(err) }
				window := map[string]any{"schema_version": 1, "kind": "p1-sender-window", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "windowId": id, "direction": direction, "grid": map[string]any{"columns": grid.Columns, "rows": grid.Rows, "epoch": snapshot.GeometryEpoch, "revision": snapshot.Revision}, "expectedSgrBase64": []byte(expected), "requestId": sends[0].RequestID, "wireRecordFile": wireFile, "startedWall": started.UTC(), "endedWall": ended.UTC(), "quietBeforeMs": started.Sub(quietBegan).Milliseconds(), "quietAfterMs": quietEnded.Sub(ended).Milliseconds(), "framesBefore": beforeFiles, "framesAfter": afterFiles}
				if err := contract.Save(id+"-window.json", window); err != nil { t.Fatal(err) }
				return before, after, expected
			}
			time.Sleep(50*time.Millisecond)
		}
		t.Fatal("numbered viewport did not move in requested direction")
		return p1Viewport{}, p1Viewport{}, ""
	}
	beforeUp, afterUp, upCenter := move("up", "row-leased", leasedGrid, leased)
	if err := leases.ReleaseClient(ctx, "row-fixture"); err != nil { t.Fatal(err) }
	restoredGrid, err := reader.Read(ctx, pid)
	if err != nil { t.Fatal(err) }
	restored := readyState(measured.Rows)
	if restoredGrid != measured || restored.GeometryEpoch <= leased.GeometryEpoch || restored.Revision == leased.Revision || notifications != 2 { t.Fatal("restoration did not invalidate measured geometry") }
	staleCount = writeCount()
	stale = d.Handle(ctx, command("row-restored-stale", leased.Revision, "down"))
	if stale.OK || stale.Phase != "not_started" || writeCount() != staleCount { t.Fatal("leased geometry admitted after restore") }
	beforeDown, afterDown, downCenter := move("down", "row-restored", restoredGrid, restored)
	for name, frame := range map[string]p1Viewport{"row-up-before.json": beforeUp, "row-up-after.json": afterUp, "row-down-before.json": beforeDown, "row-down-after.json": afterDown} { if err := contract.Save(name, frame); err != nil { t.Fatal(err) } }
	commands, err := runner.Observations()
	if err != nil { t.Fatal(err) }
	if err := contract.Save("row-lease.json", map[string]any{"schema_version": 1, "kind": "p1-row-lease", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "initial": initial, "leased": leased, "restored": restored, "native_process_info": info, "commands": commands, "resize_notifications": notifications, "stale_refusals": 2, "independent_up": upCenter, "independent_down": downCenter}); err != nil { t.Fatal(err) }
	records, err := observer.Snapshot()
	if err != nil { t.Fatal(err) }
	for index, record := range records { if err := contract.Save(fmt.Sprintf("row-wire-%03d.json", index), map[string]any{"schema_version": 1, "kind": "p1-unary-observation", "source_kind": "real", "complete": true, "truncated": false, "ambiguous": false, "skipped": false, "record": record}); err != nil { t.Fatal(err) } }
}

func TestP1OfflineMarkerMovement(t *testing.T) {
	for _, input := range []string{"", "P1 TEST USER [000]", "P1 TEST USER [003] P1 TEST USER [002]"} { if _, err := p1Markers([]byte(input)); err == nil { t.Fatal("ambiguous markers admitted") } }
	markers, err := p1Markers([]byte("P1 TEST USER [001]\nP1 TEST USER [004]"))
	if err != nil || len(markers) != 2 || markers[0] != 1 || markers[1] != 4 { t.Fatal("numbered marker parser") }
}
