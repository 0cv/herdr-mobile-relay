package webrtclink

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const maxSetupEvents = 64

type setupEvent struct {
	Elapsed     time.Duration `json:"elapsed_ns"`
	Stage       string        `json:"stage"`
	LineUfrag   string        `json:"line_ufrag,omitempty"`
	FieldUfrag  string        `json:"field_ufrag,omitempty"`
	LocalUfrag  string        `json:"local_ufrag,omitempty"`
	RemoteUfrag string        `json:"remote_ufrag,omitempty"`
	Rejected    bool          `json:"rejected,omitempty"`
	ErrorType   string        `json:"error_type,omitempty"`
	err         error
}

type setupDiagnostics struct {
	mu       sync.Mutex
	start    time.Time
	events   []setupEvent
	dropped  int
	rejected int
}

func generationHash(value string) string {
	if value == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))[:16]
}

func descriptionGeneration(description *webrtc.SessionDescription) string {
	if description != nil {
		for _, line := range strings.Split(description.SDP, "\n") {
			if strings.HasPrefix(line, "a=ice-ufrag:") {
				return generationHash(strings.TrimSpace(strings.TrimPrefix(line, "a=ice-ufrag:")))
			}
		}
	}
	return ""
}

func candidateGeneration(candidate string) string {
	fields := strings.Fields(candidate)
	for i := 8; i+1 < len(fields); i += 2 {
		if fields[i] == "ufrag" {
			return generationHash(fields[i+1])
		}
	}
	return ""
}

func (d *setupDiagnostics) record(stage string, pc *webrtc.PeerConnection, candidate string, field *string, err error) {
	event := setupEvent{Stage: stage, LineUfrag: candidateGeneration(candidate), Rejected: err != nil, err: err}
	if field != nil {
		event.FieldUfrag = generationHash(*field)
	}
	if pc != nil {
		event.LocalUfrag = descriptionGeneration(pc.LocalDescription())
		event.RemoteUfrag = descriptionGeneration(pc.RemoteDescription())
	}
	if err != nil {
		event.ErrorType = fmt.Sprintf("%T", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.start.IsZero() {
		d.start = time.Now()
	}
	event.Elapsed = time.Since(d.start)
	if event.Rejected {
		d.rejected++
	}
	if len(d.events) == maxSetupEvents {
		copy(d.events, d.events[1:])
		d.events = d.events[:maxSetupEvents-1]
		d.dropped++
	}
	d.events = append(d.events, event)
}

func peerSetupState(pc *webrtc.PeerConnection) map[string]any {
	state := map[string]any{"present": pc != nil}
	if pc == nil {
		return state
	}
	state["local_ufrag"] = descriptionGeneration(pc.LocalDescription())
	state["remote_ufrag"] = descriptionGeneration(pc.RemoteDescription())
	state["ice"] = pc.ICEConnectionState().String()
	state["connection"] = pc.ConnectionState().String()
	state["signaling"] = pc.SignalingState().String()
	state["gathering"] = pc.ICEGatheringState().String()
	if sctp := pc.SCTP(); sctp != nil {
		state["sctp"] = sctp.State().String()
		if dtls := sctp.Transport(); dtls != nil {
			state["dtls"] = dtls.State().String()
		}
	}
	return state
}

func gateSetupState(g *gate) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return map[string]any{"released": g.open, "pending": len(g.pending)}
}

func (h *harness) relayPeer(key SessionKey) *webrtc.PeerConnection {
	if session := h.manager.lookup(key); session != nil {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.pc
	}
	return nil
}

func (h *harness) recordServe(conn transport.FrameConn) {
	h.mu.Lock()
	peers := make(map[SessionKey]*browserPeer, len(h.peers))
	for key, peer := range h.peers {
		peers[key] = peer
	}
	h.mu.Unlock()
	for key, peer := range peers {
		if session := h.manager.lookup(key); session != nil {
			session.mu.Lock()
			matches, pc := session.conn == conn, session.pc
			session.mu.Unlock()
			if matches {
				peer.diagnostics.record("relay.open.callback.hub.serve", pc, "", nil, nil)
			}
		}
	}
}

func (h *harness) setupSnapshot() string {
	h.mu.Lock()
	peers := make(map[SessionKey]*browserPeer, len(h.peers))
	for key, peer := range h.peers {
		peers[key] = peer
	}
	h.mu.Unlock()
	states := make(map[int]any, len(peers))
	for key, peer := range peers {
		peer.diagnostics.mu.Lock()
		state := map[string]any{"events": append([]setupEvent(nil), peer.diagnostics.events...), "dropped": peer.diagnostics.dropped, "rejected": peer.diagnostics.rejected}
		peer.diagnostics.mu.Unlock()
		state["browser"] = peerSetupState(peer.pc)
		state["browser_channel"] = peer.dc.ReadyState().String()
		state["to_browser"] = gateSetupState(&peer.toBrowser)
		state["to_relay"] = gateSetupState(&peer.toRelay)
		if session := h.manager.lookup(key); session != nil {
			session.mu.Lock()
			pc, conn := session.pc, session.conn
			state["relay_closed"] = session.closed
			state["relay_candidates"] = session.candidates
			state["relay_pending"] = len(session.pending)
			state["relay_pair_selected"] = session.selectedRemote != ""
			session.mu.Unlock()
			state["relay"] = peerSetupState(pc)
			state["relay_datachannel_callback_reached"] = conn != nil
			if conn != nil {
				state["relay_channel"] = conn.dc.ReadyState().String()
			}
		} else {
			state["relay_session_missing"] = true
		}
		states[peer.diagnosticID] = state
	}
	data, _ := json.Marshal(states)
	return string(data)
}

func TestSetupDiagnosticsRetainAsyncCandidateError(t *testing.T) {
	h := newHarness(t, 0, nil)
	peer := newBrowserPeer(t, dataChannelLabel)
	key := SessionKey{ClientID: "fixture", RequestID: "fixture"}
	peer.diagnosticID = 1
	h.mu.Lock()
	h.peers[key] = peer
	h.mu.Unlock()
	peer.toBrowser.release()
	if err := peer.pc.Close(); err != nil {
		t.Fatal(err)
	}
	h.routeCandidate(key, Candidate{Candidate: "candidate:1 1 udp 1 192.0.2.1 1234 typ host ufrag private-generation"})
	peer.diagnostics.mu.Lock()
	rejected := peer.diagnostics.rejected
	retained := false
	for _, event := range peer.diagnostics.events {
		retained = retained || event.err != nil
	}
	peer.diagnostics.mu.Unlock()
	if rejected != 1 || !retained {
		t.Fatal("asynchronous candidate rejection discarded")
	}
	snapshot := h.setupSnapshot()
	if !strings.Contains(snapshot, `"rejected":1`) || strings.Contains(snapshot, "192.0.2.1") || strings.Contains(snapshot, "private-generation") {
		t.Fatal("timeout snapshot lost or exposed candidate evidence")
	}
}

func TestSetupDiagnosticsBoundedAndPrivate(t *testing.T) {
	var diagnostics setupDiagnostics
	field := "private-generation"
	candidate := "candidate:1 1 udp 1 192.0.2.1 1234 typ host ufrag private-generation"
	failure := errors.New("must not export raw SDP, address or credentials")
	for range maxSetupEvents + 7 {
		diagnostics.record("candidate.applied", nil, candidate, &field, failure)
	}
	if len(diagnostics.events) != maxSetupEvents || diagnostics.dropped != 7 || diagnostics.rejected != maxSetupEvents+7 || !errors.Is(diagnostics.events[0].err, failure) {
		t.Fatal("candidate errors or bounds lost")
	}
	event := diagnostics.events[0]
	if event.LineUfrag == "" || event.LineUfrag != event.FieldUfrag || candidateGeneration("candidate:1 1 udp 1 192.0.2.1 1234 typ host") != "" {
		t.Fatal("candidate generation evidence lost")
	}
	description := &webrtc.SessionDescription{SDP: "a=ice-ufrag:private-generation\r\na=ice-pwd:private-password\r\n"}
	if descriptionGeneration(description) != event.LineUfrag {
		t.Fatal("SDP and candidate generations cannot be correlated")
	}
	encoded, err := json.Marshal(diagnostics.events)
	if err != nil || strings.Contains(string(encoded), "private-generation") || strings.Contains(string(encoded), "192.0.2.1") || strings.Contains(string(encoded), failure.Error()) || strings.Contains(string(encoded), "private-password") {
		t.Fatal("diagnostics exposed sensitive inputs")
	}
}
