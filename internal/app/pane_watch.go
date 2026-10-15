package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const defaultPaneWatchInterval = 250 * time.Millisecond

// paneResizeSettleWindow bounds how long after an actual leased-width change
// pane frames are marked resize_settling. Full-screen agents re-render their
// transcript on SIGWINCH and can push redrawn rows into the scrollback for a
// couple of seconds; observed up to ~2s for omp under load.
const paneResizeSettleWindow = 3 * time.Second

type paneWatchFrame struct {
	content             string
	contentFingerprint  string
	frameFingerprint    string
	classificationAgent string
	resizeSettling      bool
	scrollRevision      string
}

type paneWatch struct {
	client   *transport.ClientConn
	paneID   string
	target   protocol.TargetRef
	lines    int
	format   string
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	refresh  chan struct{}

	mu               sync.Mutex
	acknowledged     *paneWatchFrame
	pending          *paneWatchFrame
	probeFingerprint string
	forceRefresh     bool
}

func (s *Server) startPaneWatch(client *transport.ClientConn, message map[string]any) {
	paneID, _ := message["pane_id"].(string)
	if paneID == "" {
		return
	}
	lines := messageInt(message["lines"], 30)
	if lines < 1 {
		lines = 1
	} else if lines > history.MaxLines {
		lines = history.MaxLines
	}
	format, _ := message["format"].(string)
	if format != "ansi" {
		format = "text"
	}
	target := protocol.TargetRef{ServerSessionID: "primary", PaneID: paneID}
	if inbound, err := protocol.DecodeMap(message); err == nil && inbound.Target != nil {
		target = *inbound.Target
	}
	interval := requestedPaneWatchInterval(message["interval_ms"])
	ctx, cancel := context.WithCancel(client.Context())
	watch := &paneWatch{
		client:   client,
		paneID:   paneID,
		target:   target,
		lines:    lines,
		format:   format,
		interval: interval,
		ctx:      ctx,
		cancel:   cancel,
		refresh:  make(chan struct{}, 1),
	}

	s.paneWatchMu.Lock()
	if previous := s.paneWatches[client.ID()]; previous != nil {
		previous.cancel()
	}
	s.paneWatches[client.ID()] = watch
	s.paneWatchMu.Unlock()

	knownFingerprint, _ := message["content_fingerprint"].(string)
	go s.runPaneWatch(watch, knownFingerprint)
}

func (s *Server) stopPaneWatch(clientID, paneID string) {
	s.paneWatchMu.Lock()
	watch := s.paneWatches[clientID]
	if watch != nil && (paneID == "" || watch.paneID == paneID) {
		delete(s.paneWatches, clientID)
		watch.cancel()
	}
	s.paneWatchMu.Unlock()
}

func (s *Server) runPaneWatch(watch *paneWatch, knownFingerprint string) {
	defer func() {
		s.paneWatchMu.Lock()
		if s.paneWatches[watch.client.ID()] == watch {
			delete(s.paneWatches, watch.client.ID())
		}
		s.paneWatchMu.Unlock()
	}()

	for {
		if !s.paneWatchCurrent(watch) {
			return
		}
		response, frame := s.readPaneWatchFrame(watch)
		if frame != nil {
			var acknowledged *paneWatchFrame
			if knownFingerprint == frame.contentFingerprint {
				acknowledged = &paneWatchFrame{
					content:            frame.content,
					contentFingerprint: knownFingerprint,
				}
			}
			message := paneWatchUpdate(response, acknowledged, frame)
			watch.mu.Lock()
			if message == nil {
				watch.acknowledged = frame
			} else {
				watch.pending = frame
			}
			watch.mu.Unlock()
			if message != nil {
				s.hub.Send(watch.client, message)
			}
			break
		}
		select {
		case <-watch.ctx.Done():
			return
		case <-time.After(watch.interval):
		}
	}

	s.pollPaneWatch(watch)
	ticker := time.NewTicker(watch.interval)
	defer ticker.Stop()
	for {
		select {
		case <-watch.ctx.Done():
			return
		case <-ticker.C:
			s.pollPaneWatch(watch)
		case <-watch.refresh:
			s.pollPaneWatch(watch)
		}
	}
}

func (s *Server) pollPaneWatch(watch *paneWatch) {
	if !s.paneWatchCurrent(watch) {
		return
	}
	state := s.dispatcher.ScrollState(watch.ctx, watch.paneID, watch.client.ID(), true)
	watch.mu.Lock()
	force := watch.forceRefresh
	watch.forceRefresh = false
	current := watch.acknowledged
	if watch.pending != nil {
		current = watch.pending
	}
	stateChanged := state != nil && (current == nil || current.scrollRevision != state.Revision)
	if watch.pending != nil && !stateChanged && !force {
		watch.mu.Unlock()
		return
	}
	if stateChanged || force {
		watch.pending = nil
	}
	previousProbe := watch.probeFingerprint
	acknowledged := watch.acknowledged
	watch.mu.Unlock()

	probe := s.dispatcher.HandleProbePane(watch.ctx, watchMessage(watch))
	probeContent, ok := successfulPaneContent(probe)
	if !ok {
		return
	}
	probeFingerprint := paneFingerprint(probeContent)
	classificationAgentID, _ := s.agentInfo(watch.paneID)
	if !paneWatchNeedsFrameRead(
		previousProbe,
		probeFingerprint,
		acknowledged,
		classificationAgentID,
		stateChanged || force,
	) {
		return
	}

	response, frame := s.readPaneWatchFrame(watch)
	if frame == nil || !s.paneWatchCurrent(watch) {
		return
	}
	watch.mu.Lock()
	if watch.pending != nil {
		watch.mu.Unlock()
		return
	}
	watch.probeFingerprint = probeFingerprint
	acknowledged = watch.acknowledged
	message := paneWatchUpdate(response, acknowledged, frame)
	if message == nil {
		watch.acknowledged = frame
		watch.mu.Unlock()
		return
	}
	watch.pending = frame
	watch.mu.Unlock()
	s.hub.Send(watch.client, message)
}

func (s *Server) readPaneWatchFrame(watch *paneWatch) (map[string]any, *paneWatchFrame) {
	message := watchMessage(watch)
	s.applyPaneReadLease(message)
	response := s.preparePaneResponse(message, s.dispatcher.HandleReadPane(watch.ctx, message))
	response["target"] = watch.target
	content, ok := successfulPaneContent(response)
	if !ok {
		return response, nil
	}
	contentFingerprint := paneFingerprint(content)
	frameFingerprint := paneFrameFingerprint(response)
	classificationAgentID, _ := s.agentInfo(watch.paneID)
	resizeSettling, _ := response["resize_settling"].(bool)
	response["content_fingerprint"] = contentFingerprint
	return response, &paneWatchFrame{
		content:             content,
		contentFingerprint:  contentFingerprint,
		frameFingerprint:    frameFingerprint,
		classificationAgent: classificationAgentID,
		resizeSettling:      resizeSettling,
		scrollRevision:      scrollRevision(response),
	}
}

func paneWatchNeedsFrameRead(
	previousProbe, probeFingerprint string,
	acknowledged *paneWatchFrame,
	classificationAgentID string,
	stateChanged bool,
) bool {
	if stateChanged {
		return true
	}
	if previousProbe == "" || probeFingerprint != previousProbe {
		return true
	}
	return acknowledged != nil &&
		(acknowledged.resizeSettling ||
			acknowledged.classificationAgent != classificationAgentID)
}

func scrollRevision(response map[string]any) string {
	if state, ok := response["scrolling"].(*protocol.ScrollState); ok && state != nil {
		return state.Revision
	}
	return ""
}

func (s *Server) refreshScrollWatch(owner, pane string) {
	s.paneWatchMu.Lock()
	watch := s.paneWatches[owner]
	s.paneWatchMu.Unlock()
	if watch == nil || watch.paneID != pane {
		return
	}
	queuePaneWatchRefresh(watch)
}

func paneInputChangesScreen(action string) bool {
	switch action {
	case "submit_prompt", "send_text", "send_filter_text", "send_keys", "send_input", "send_secret", "respond", "answer_question", "clarify_question", "navigate_question", "scroll_agent":
		return true
	default:
		return false
	}
}

func (s *Server) refreshPaneWatchesAfterInput(action, pane string, result *coordinator.CommandResult) {
	if result != nil && (result.OK || result.Phase == "dispatched_unknown") && paneInputChangesScreen(action) {
		s.refreshPaneWatches(pane)
	}
}

func (s *Server) refreshPaneWatches(pane string) {
	s.paneWatchMu.Lock()
	defer s.paneWatchMu.Unlock()
	for _, watch := range s.paneWatches {
		if watch.paneID == pane {
			queuePaneWatchRefresh(watch)
		}
	}
}

func queuePaneWatchRefresh(watch *paneWatch) {
	watch.mu.Lock()
	watch.forceRefresh = true
	watch.mu.Unlock()
	select {
	case watch.refresh <- struct{}{}:
	default:
	}
}

func (s *Server) paneWatchCurrent(watch *paneWatch) bool {
	if watch.target.TerminalID != "" && validateExactPaneTarget(s.state, protocol.Inbound{PaneID: watch.paneID, Target: &watch.target}, true) != nil {
		watch.cancel()
		s.dispatcher.ForgetScrollOwner(watch.client.ID())
		return false
	}
	s.paneWatchMu.Lock()
	defer s.paneWatchMu.Unlock()
	return s.paneWatches[watch.client.ID()] == watch
}

func (s *Server) handlePaneApplied(client *transport.ClientConn, message map[string]any) {
	paneID, _ := message["pane_id"].(string)
	fingerprint, _ := message["content_fingerprint"].(string)
	frameRevision, _ := message["frame_revision"].(string)
	s.paneWatchMu.Lock()
	watch := s.paneWatches[client.ID()]
	s.paneWatchMu.Unlock()
	if watch == nil || watch.paneID != paneID || fingerprint == "" {
		return
	}
	watch.mu.Lock()
	if watch.pending != nil && watch.pending.contentFingerprint == fingerprint && watch.pending.frameFingerprint == frameRevision {
		watch.acknowledged = watch.pending
		watch.pending = nil
		watch.mu.Unlock()
		return
	}
	if watch.acknowledged != nil && watch.acknowledged.contentFingerprint == fingerprint && watch.acknowledged.frameFingerprint == frameRevision {
		watch.mu.Unlock()
		return
	}
	watch.mu.Unlock()
	s.hub.Send(client, map[string]any{"type": "pane_resync", "pane_id": paneID, "target": watch.target})
}

func requestedPaneWatchInterval(value any) time.Duration {
	milliseconds := messageInt(value, int(defaultPaneWatchInterval/time.Millisecond))
	switch milliseconds {
	case 100, 250, 500, 1_000:
		return time.Duration(milliseconds) * time.Millisecond
	default:
		return defaultPaneWatchInterval
	}
}

func watchMessage(watch *paneWatch) map[string]any {
	return map[string]any{
		"pane_id":       watch.paneID,
		"lines":         watch.lines,
		"format":        watch.format,
		"target":        watch.target,
		"_scroll_owner": watch.client.ID(),
	}
}

func (s *Server) applyPaneReadLease(message map[string]any) {
	delete(message, "terminal_columns")
	delete(message, "terminal_rows")
	if s.paneSizeM == nil {
		return
	}
	paneID, _ := message["pane_id"].(string)
	if columns, ok := s.paneSizeM.ActiveColumns(paneID); ok {
		message["terminal_columns"] = columns
		if rows, rowsOK := s.paneSizeM.ActiveRows(paneID); rowsOK {
			message["terminal_rows"] = rows
		}
	}
}

func paneWatchUpdate(
	response map[string]any,
	acknowledged, current *paneWatchFrame,
) map[string]any {
	if acknowledged != nil && acknowledged.frameFingerprint != "" &&
		acknowledged.frameFingerprint == current.frameFingerprint {
		return nil
	}
	if acknowledged == nil {
		response["ack_required"] = true
		return response
	}
	if acknowledged.contentFingerprint == current.contentFingerprint {
		// Preserve the frame through a tiny copy segment. Released clients
		// reconstruct an empty segment list as empty terminal content.
		segments := []panedelta.Segment{{
			CopyLines: strings.Count(current.content, "\n") + 1,
		}}
		return paneDeltaResponse(response, acknowledged.contentFingerprint, segments)
	}
	segments := panedelta.Build(acknowledged.content, current.content)
	if panedelta.Efficient(segments, current.content) {
		return paneDeltaResponse(response, acknowledged.contentFingerprint, segments)
	}
	response["ack_required"] = true
	return response
}

func paneDeltaResponse(response map[string]any, baseFingerprint string, segments []panedelta.Segment) map[string]any {
	delta := make(map[string]any, len(response)+2)
	for key, value := range response {
		if key != "content" {
			delta[key] = value
		}
	}
	delta["type"] = "pane_delta"
	delta["base_fingerprint"] = baseFingerprint
	delta["segments"] = segments
	return delta
}
