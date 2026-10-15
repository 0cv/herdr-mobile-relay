package coordinator

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/piscroll"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

func NewDispatcherWithPiScroll(client *herdr.Client, state *State, journal *activity.Journal, logger *slog.Logger, socket string, runner panesize.CommandRunner) *Dispatcher {
	d := NewDispatcher(client, state, journal, logger)
	d.enablePiScroll(socket, runner)
	return d
}

func (d *Dispatcher) EnablePiScroll(socket string) {
	d.enablePiScroll(socket, nil)
}

func (d *Dispatcher) enablePiScroll(socket string, runner panesize.CommandRunner) {
	d.readMu.Lock()
	defer d.readMu.Unlock()
	if d.piResolver != nil {
		return
	}
	reader := panesize.NewReader(runner, "")
	reader.Observe = func(ctx context.Context, measurement panesize.CommandMeasurement) {
		d.logger.Debug("Pi geometry measurement", "request_id", piscroll.IntentID(ctx), "command", measurement.Name, "elapsed_ms", float64(measurement.Duration.Microseconds())/1000, "failed", measurement.Failed)
	}
	d.piResolver = &piscroll.PiResolver{
		Process: d.herdr.SocketPaneProcessInfo, Bridge: pibridge.QueryScrollState, Catalog: pibridge.Query, Geometry: reader.Read,
		Instance: func() (string, error) { return pibridge.Instance(socket) }, Supported: d.herdr.SupportsPiScrollSocket,
		Observe: func(metrics piscroll.ResolveMetrics) {
			for _, phase := range metrics.Phases {
				d.logger.Debug("Pi phase timing", "request_id", metrics.IntentID, "operation", metrics.Operation, "phase", phase.Phase, "elapsed_ms", float64(phase.Duration.Microseconds())/1000)
			}
			d.logger.Debug("Pi runtime evidence", "request_id", metrics.IntentID, "operation", metrics.Operation, "phase", metrics.Phase, "elapsed_ms", metrics.Duration.Milliseconds(), "process_reads", metrics.ProcessReads, "bridge_queries", metrics.BridgeQueries, "geometry_reads", metrics.GeometryReads, "failed", metrics.Failed, "terminal_present", metrics.TerminalPresent, "session_present", metrics.SessionPresent, "socket_supported", metrics.SocketSupported, "target_current", metrics.TargetCurrent, "socket_reason", d.herdr.CapabilityStatus().Feature(herdr.FeaturePiScrollSocket).Reason)
		},
		Current: func(target piscroll.Target) bool {
			current, ok := d.scrollTarget(target.Ref.PaneID)
			return ok && current == target
		},
	}
	d.scroll = piscroll.New(d.piResolver)
	d.piCatalogSlots = make(chan struct{}, 8)
}

func (d *Dispatcher) PiCatalog(ctx context.Context, pane string) (slashcmd.Catalog, error) {
	target, ok := d.scrollTarget(pane)
	if !ok || d.piResolver == nil {
		return slashcmd.Catalog{}, errors.New("Pi target unavailable")
	}
	select {
	case d.piCatalogSlots <- struct{}{}:
		defer func() { <-d.piCatalogSlots }()
	default:
		return slashcmd.Catalog{}, errors.New("Pi discovery busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := d.piResolver.ResolveCatalog(ctx, target)
	if err != nil {
		return slashcmd.Catalog{}, err
	}
	return slashcmd.MergePiRuntime(response.Commands, response.Status, response.Truncated), nil
}

func (d *Dispatcher) SetScrollManager(manager *piscroll.Manager) { d.scroll = manager }
func (d *Dispatcher) ForgetScrollOwner(owner string) {
	if d.scroll != nil {
		d.scroll.ForgetOwner(owner)
	}
}
func (d *Dispatcher) InvalidateScrollGeometry(pane string) {
	if d.scroll != nil {
		d.scroll.InvalidateGeometry(pane)
	}
}

func (d *Dispatcher) scrollTarget(pane string) (piscroll.Target, bool) {
	a, ok := d.state.Agent(pane)
	if !ok {
		return piscroll.Target{}, false
	}
	session := a.ServerSessionID
	if session == "" {
		session = "primary"
	}
	return piscroll.Target{Ref: protocol.TargetRef{ServerSessionID: session, PaneID: pane, TerminalID: a.TerminalID, Generation: a.Generation, AgentSessionID: a.SessionID}, Agent: a.Agent}, true
}

func (d *Dispatcher) ScrollState(ctx context.Context, pane, owner string, refresh bool) *protocol.ScrollState {
	target, ok := d.scrollTarget(pane)
	if d.scroll == nil || !ok || !piscroll.IsPi(target.Agent) {
		return nil
	}
	var state protocol.ScrollState
	if refresh {
		ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		state = d.scroll.Refresh(ctx, target, owner)
	} else {
		state = d.scroll.Snapshot(target, owner)
	}
	return &state
}

func (d *Dispatcher) handleScrollSelection(ctx context.Context, received time.Time, requestID, pane string, message map[string]any) *CommandResult {
	const action = "set_scroll_selection"
	target, ok := d.scrollTarget(pane)
	owner := stringValue(message, "_scroll_owner")
	if d.scroll == nil || !ok || owner == "" || requestID == "" {
		return d.failErr(requestID, action, pane, herdr.ErrNotStarted)
	}
	payload := struct {
		Target              piscroll.Target
		Selection, Revision string
	}{target, stringValue(message, "selection"), stringValue(message, "state_revision")}
	key := "scroll-selection\x00" + owner + "\x00" + pane + "\x00" + requestID
	if replay, found, err := d.scheduler.ReplayLedger(key, hashPayload(payload)); found && replay != nil || err != nil {
		if err != nil {
			return d.failErr(requestID, action, pane, errors.Join(herdr.ErrNotStarted, err))
		}
		return replay
	}
	return d.schedule(ctx, ScheduleOptions{Command: d.command(ctx, received, requestID, CommandScrollSelection, pane, 500*time.Millisecond, payload), LedgerKey: key, PayloadHash: hashPayload(payload)}, EffectFunc(func(effectCtx context.Context, _ WorkerToken) EffectResult {
		effectCtx, cancel := context.WithCancel(effectCtx)
		stop := context.AfterFunc(ctx, cancel)
		defer cancel()
		defer stop()
		if ctx.Err() != nil {
			cancel()
		}
		current := d.ScrollState(effectCtx, pane, owner, true)
		if current == nil || current.Revision != payload.Revision {
			result := d.failErr(requestID, action, pane, herdr.ErrNotStarted)
			result.Data = map[string]any{"scrolling": current}
			return EffectResult{Result: result}
		}
		if err := ctx.Err(); err != nil {
			return EffectResult{Result: d.failErr(requestID, action, pane, errors.Join(herdr.ErrNotStarted, err))}
		}
		if err := herdr.CheckDispatch(effectCtx); err != nil {
			return EffectResult{Result: d.failErr(requestID, action, pane, errors.Join(herdr.ErrNotStarted, err))}
		}
		state, err := d.scroll.SelectAtRevision(target, owner, payload.Selection, payload.Revision)
		result := completed(requestID, action, pane, map[string]any{"scrolling": state})
		if err != nil {
			result = d.failErr(requestID, action, pane, errors.Join(herdr.ErrNotStarted, err))
			result.Data = map[string]any{"scrolling": state}
		}
		return EffectResult{Result: result}
	}))
}

func (d *Dispatcher) handleScroll(ctx context.Context, received time.Time, requestID, pane string, message map[string]any) *CommandResult {
	const action = "scroll_agent"
	target, ok := d.scrollTarget(pane)
	owner, revision, direction := stringValue(message, "_scroll_owner"), stringValue(message, "state_revision"), stringValue(message, "direction")
	if d.scroll == nil || !ok || !piscroll.IsPi(target.Agent) || owner == "" || revision == "" || (direction != "up" && direction != "down") {
		return d.failErr(requestID, action, pane, errors.Join(herdr.ErrNotStarted, errors.New("invalid Pi wheel intent")))
	}
	key := "scroll\x00" + owner + "\x00" + pane + "\x00" + requestID
	payload := struct {
		Target              piscroll.Target
		Direction, Revision string
	}{target, direction, revision}
	if replay, found, err := d.scheduler.ReplayLedger(key, hashPayload(payload)); found && replay != nil || err != nil {
		if err != nil {
			return d.failErr(requestID, action, pane, errors.Join(herdr.ErrNotStarted, err))
		}
		return replay
	}
	admission := received.Add(250 * time.Millisecond)
	result := d.schedule(ctx, ScheduleOptions{
		Command: d.command(ctx, received, requestID, CommandScroll, pane, 500*time.Millisecond, payload), LedgerKey: key, PayloadHash: hashPayload(payload),
		Reserve: func() (func(), error) {
			started := time.Now()
			release, err := d.scroll.Begin(pane)
			d.logger.Debug("Pi phase timing", "request_id", requestID, "operation", action, "phase", "slot_admission", "elapsed_ms", float64(time.Since(started).Microseconds())/1000, "failed", err != nil)
			if err != nil {
				return nil, errors.Join(herdr.ErrNotStarted, err)
			}
			return release, nil
		},
	}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		d.logger.Debug("Pi phase timing", "request_id", requestID, "operation", action, "phase", "receipt_to_effect", "elapsed_ms", float64(time.Since(received).Microseconds())/1000)
		effectCtx, cancelEffect := context.WithCancel(effectCtx)
		stopParent := context.AfterFunc(ctx, cancelEffect)
		defer cancelEffect()
		defer stopParent()
		if ctx.Err() != nil {
			cancelEffect()
		}
		preflight, cancel := context.WithDeadline(piscroll.WithIntentID(effectCtx, requestID), admission)
		err := preflight.Err()
		if err == nil {
			err = herdr.CheckDispatch(preflight)
		}
		if err == nil {
			err = d.scroll.Validate(preflight, target, true)
		}
		cancel()
		preflightElapsed := time.Since(received)
		var writeStarted time.Time
		defer func() {
			var writeElapsed, ackElapsed time.Duration
			if !writeStarted.IsZero() {
				writeElapsed = writeStarted.Sub(received)
				ackElapsed = time.Since(writeStarted)
				d.logger.Debug("Pi phase timing", "request_id", requestID, "operation", action, "phase", "receipt_to_dispatch", "elapsed_ms", float64(writeElapsed.Microseconds())/1000)
			}
			d.logger.Debug("Pi wheel timing", "request_id", requestID, "preflight_ms", float64(preflightElapsed.Microseconds())/1000, "write_started", !writeStarted.IsZero(), "receipt_to_write_ms", float64(writeElapsed.Microseconds())/1000, "ack_ms", float64(ackElapsed.Microseconds())/1000, "total_ms", float64(time.Since(received).Microseconds())/1000)
		}()
		state := d.scroll.Snapshot(target, owner)
		if err == nil && (state.Revision != revision || state.Route != "application" || !state.Available) {
			err = errors.New("scroll evidence changed or unavailable")
		}
		if err == nil {
			report, reportErr := piscroll.Report(direction, panesize.Grid{Rows: state.Rows, Columns: state.Columns})
			err = reportErr
			if err == nil {
				ack, done := context.WithTimeout(effectCtx, 250*time.Millisecond)
				ack = herdr.WithDispatchCheck(ack, func() error {
					if err := ctx.Err(); err != nil {
						return err
					}
					if !time.Now().Before(admission) {
						return context.DeadlineExceeded
					}
					current, ok := d.scrollTarget(pane)
					if !ok || current != target || d.piResolver != nil && !d.piResolver.Supported() || d.scroll.Snapshot(target, owner).Revision != revision {
						return errors.New("scroll intent cancelled")
					}
					writeStarted = time.Now()
					return nil
				})
				err = d.herdr.SendRawText(ack, pane, report)
				if !writeStarted.IsZero() {
					d.logger.Debug("Pi phase timing", "request_id", requestID, "operation", action, "phase", "acknowledgement", "elapsed_ms", float64(time.Since(writeStarted).Microseconds())/1000, "failed", err != nil)
				}
				done()
			}
		}
		if err != nil {
			if !errors.Is(err, herdr.ErrDispatchedUnknown) {
				err = errors.Join(herdr.ErrNotStarted, err)
			} else {
				d.scroll.Refuse(target)
			}
			outcome := err
			if errors.Is(err, herdr.ErrDispatchedUnknown) {
				outcome = herdr.ErrDispatchedUnknown
			}
			failed := d.failErr(requestID, action, pane, outcome)
			failed.Data = map[string]any{"scrolling": d.scroll.Snapshot(target, owner)}
			return EffectResult{Result: failed}
		}
		return EffectResult{Result: completed(requestID, action, pane, map[string]any{"scrolling": state})}
	}))
	d.logger.Debug("Pi scroll admission", "elapsed_ms", time.Since(received).Milliseconds(), "phase", result.Phase, "pane_id", pane)
	if result.OK {
		d.wake()
	}
	return result
}
