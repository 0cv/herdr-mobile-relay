package app

import (
	"context"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/history"
	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const defaultPaneWatchInterval = 250 * time.Millisecond

type paneWatchFrame struct {
	content     string
	fingerprint string
}

type paneWatch struct {
	client   *transport.ClientConn
	paneID   string
	lines    int
	format   string
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc

	mu               sync.Mutex
	acknowledged     *paneWatchFrame
	pending          *paneWatchFrame
	probeFingerprint string
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
	interval := requestedPaneWatchInterval(message["interval_ms"])
	ctx, cancel := context.WithCancel(client.Context())
	watch := &paneWatch{
		client:   client,
		paneID:   paneID,
		lines:    lines,
		format:   format,
		interval: interval,
		ctx:      ctx,
		cancel:   cancel,
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
			watch.mu.Lock()
			if knownFingerprint == frame.fingerprint {
				watch.acknowledged = frame
			} else {
				response["ack_required"] = true
				watch.pending = frame
			}
			watch.mu.Unlock()
			if knownFingerprint != frame.fingerprint {
				s.hub.Send(watch.client, response)
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
		}
	}
}

func (s *Server) pollPaneWatch(watch *paneWatch) {
	if !s.paneWatchCurrent(watch) {
		return
	}
	watch.mu.Lock()
	if watch.pending != nil {
		watch.mu.Unlock()
		return
	}
	previousProbe := watch.probeFingerprint
	watch.mu.Unlock()

	probe := s.dispatcher.HandleProbePane(watch.ctx, watchMessage(watch))
	probeContent, ok := successfulPaneContent(probe)
	if !ok {
		return
	}
	probeFingerprint := paneFingerprint(probeContent)
	if previousProbe != "" && probeFingerprint == previousProbe {
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
	acknowledged := watch.acknowledged
	if acknowledged != nil && acknowledged.fingerprint == frame.fingerprint {
		watch.mu.Unlock()
		return
	}
	message := response
	if acknowledged != nil {
		segments := panedelta.Build(acknowledged.content, frame.content)
		if panedelta.Efficient(segments, frame.content) {
			message = paneDeltaResponse(response, acknowledged.fingerprint, segments)
		} else {
			message["ack_required"] = true
		}
	} else {
		message["ack_required"] = true
	}
	watch.pending = frame
	watch.mu.Unlock()
	s.hub.Send(watch.client, message)
}

func (s *Server) readPaneWatchFrame(watch *paneWatch) (map[string]any, *paneWatchFrame) {
	message := watchMessage(watch)
	s.applyPaneReadLease(message)
	response := s.preparePaneResponse(message, s.dispatcher.HandleReadPane(watch.ctx, message))
	content, ok := successfulPaneContent(response)
	if !ok {
		return response, nil
	}
	fingerprint := paneFingerprint(content)
	response["content_fingerprint"] = fingerprint
	return response, &paneWatchFrame{content: content, fingerprint: fingerprint}
}

func (s *Server) paneWatchCurrent(watch *paneWatch) bool {
	s.paneWatchMu.Lock()
	defer s.paneWatchMu.Unlock()
	return s.paneWatches[watch.client.ID()] == watch
}

func (s *Server) handlePaneApplied(client *transport.ClientConn, message map[string]any) {
	paneID, _ := message["pane_id"].(string)
	fingerprint, _ := message["content_fingerprint"].(string)
	s.paneWatchMu.Lock()
	watch := s.paneWatches[client.ID()]
	s.paneWatchMu.Unlock()
	if watch == nil || watch.paneID != paneID || fingerprint == "" {
		return
	}
	watch.mu.Lock()
	if watch.pending != nil && watch.pending.fingerprint == fingerprint {
		watch.acknowledged = watch.pending
		watch.pending = nil
		watch.mu.Unlock()
		return
	}
	if watch.acknowledged != nil && watch.acknowledged.fingerprint == fingerprint {
		watch.mu.Unlock()
		return
	}
	watch.mu.Unlock()
	s.hub.Send(client, map[string]any{"type": "pane_resync", "pane_id": paneID})
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
		"pane_id": watch.paneID,
		"lines":   watch.lines,
		"format":  watch.format,
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
