package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/0cv/herdr-mobile-relay/internal/webrtclink"
	"github.com/pion/webrtc/v4"
)

var framePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
	0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
	0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x00, 0x02, 0x00, 0x01, 0xe2, 0x21, 0xbc,
	0x33, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
	0x44, 0xae, 0x42, 0x60, 0x82,
}

func resultOf(t *testing.T, response map[string]any, messageType string) map[string]any {
	t.Helper()
	result, _ := response["result"].(map[string]any)
	if response["type"] != messageType || response["request_id"] != "req-1" || result == nil {
		t.Fatalf("response = %#v, want a %s result", response, messageType)
	}
	return result
}

func uploadErrorCode(t *testing.T, response map[string]any, messageType string) string {
	t.Helper()
	apiError, _ := response["error"].(map[string]any)
	if response["type"] != messageType || response["request_id"] != "req-1" || apiError == nil {
		t.Fatalf("response = %#v, want a %s error", response, messageType)
	}
	code, _ := apiError["code"].(string)
	return code
}

// uploadFrame sends an upload request the way the app does: the target is
// nested and no pane_id sits beside it.
func (e *frameEnv) uploadFrame(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	fields["pane_id"] = nil
	return e.exchange(t, fields)
}

func (e *frameEnv) beginUpload(t *testing.T, target map[string]any) string {
	t.Helper()
	response := e.uploadFrame(t, map[string]any{
		"type": "upload_begin", "target": target,
		"files": []map[string]any{{"name": "shot.png", "media_type": "image/png", "bytes": len(framePNG)}},
	})
	uploadID, _ := resultOf(t, response, "upload_begin_result")["upload_id"].(string)
	if uploadID == "" {
		t.Fatalf("begin result = %#v, want an upload id", response)
	}
	return uploadID
}

func (e *frameEnv) uploadChunk(t *testing.T, target map[string]any, uploadID string, sequence int) map[string]any {
	t.Helper()
	digest := sha256.Sum256(framePNG)
	return e.uploadFrame(t, map[string]any{
		"type": "upload_chunk", "target": target, "upload_id": uploadID, "file_index": 0, "sequence": sequence,
		"data": base64.StdEncoding.EncodeToString(framePNG), "sha256": fmt.Sprintf("%x", digest),
	})
}

func (e *frameEnv) finishUpload(t *testing.T, target map[string]any, uploadID string) map[string]any {
	t.Helper()
	digest := sha256.Sum256(framePNG)
	return e.uploadFrame(t, map[string]any{
		"type": "upload_finish", "target": target, "upload_id": uploadID,
		"files": []map[string]any{{"file_index": 0, "sha256": fmt.Sprintf("%x", digest)}},
	})
}

func (e *frameEnv) uploadAttachment(t *testing.T, target map[string]any) string {
	t.Helper()
	uploadID := e.beginUpload(t, target)
	e.uploadChunk(t, target, uploadID, 0)
	finished := resultOf(t, e.finishUpload(t, target, uploadID), "upload_finish_result")
	attachments, _ := finished["attachments"].([]any)
	attachment, _ := attachments[0].(map[string]any)
	ref, _ := attachment["ref"].(string)
	if ref == "" {
		t.Fatalf("finish result = %#v, want an attachment reference", finished)
	}
	return ref
}

func TestFrameUploadLifecycle(t *testing.T) {
	e := newFrameEnv(t)
	target := e.target(t, "pane-1")
	digest := fmt.Sprintf("%x", sha256.Sum256(framePNG))

	begin := resultOf(t, e.uploadFrame(t, map[string]any{
		"type": "upload_begin", "target": target,
		"files": []map[string]any{{"name": "shot.png", "media_type": "image/png", "bytes": len(framePNG)}},
	}), "upload_begin_result")
	uploadID, _ := begin["upload_id"].(string)
	if uploadID == "" || begin["chunk_bytes"] == nil || begin["limits"] == nil {
		t.Fatalf("begin result = %#v", begin)
	}

	chunk := resultOf(t, e.uploadChunk(t, target, uploadID, 0), "upload_chunk_result")
	if chunk["file_index"] != float64(0) || chunk["next_sequence"] != float64(1) || chunk["received_bytes"] != float64(len(framePNG)) {
		t.Fatalf("chunk result = %#v", chunk)
	}

	finished := resultOf(t, e.finishUpload(t, target, uploadID), "upload_finish_result")
	attachments, _ := finished["attachments"].([]any)
	if len(attachments) != 1 {
		t.Fatalf("finish result = %#v", finished)
	}
	attachment, _ := attachments[0].(map[string]any)
	if attachment["name"] != "shot.png" || attachment["media_type"] != "image/png" || attachment["sha256"] != digest || attachment["ref"] == "" {
		t.Fatalf("attachment = %#v", attachment)
	}
	if _, leaked := attachment["path"]; leaked {
		t.Fatalf("attachment leaked its host path: %#v", attachment)
	}

	cancelled := e.beginUpload(t, target)
	resultOf(t, e.uploadFrame(t, map[string]any{"type": "upload_cancel", "target": target, "upload_id": cancelled}), "upload_cancel_result")
	if code := uploadErrorCode(t, e.uploadChunk(t, target, cancelled, 0), "upload_chunk_result"); code != "attachment_upload_state_unknown" {
		t.Fatalf("chunk after cancel: error code = %s", code)
	}
}

func TestFrameUploadRefusals(t *testing.T) {
	e := newFrameEnv(t)
	target := e.target(t, "pane-1")

	if code := uploadErrorCode(t, e.uploadFrame(t, map[string]any{
		"type": "upload_begin", "target": target,
		"files": []map[string]any{{"name": "evil.exe", "media_type": "application/octet-stream", "bytes": 3}},
	}), "upload_begin_result"); code != "attachment_unknown_mime" {
		t.Fatalf("a disallowed type: error code = %s", code)
	}

	stale := e.target(t, "pane-1")
	stale["generation"] = 99
	if code := uploadErrorCode(t, e.uploadFrame(t, map[string]any{
		"type": "upload_begin", "target": stale,
		"files": []map[string]any{{"name": "shot.png", "media_type": "image/png", "bytes": len(framePNG)}},
	}), "upload_begin_result"); code != "attachment_upload_state_unknown" {
		t.Fatalf("a stale target: error code = %s", code)
	}

	if code := uploadErrorCode(t, e.uploadFrame(t, map[string]any{
		"type": "upload_begin", "target": target, "files": "shot.png",
	}), "upload_begin_result"); code != "attachment_upload_failed" {
		t.Fatalf("files of the wrong type: error code = %s", code)
	}

	uploadID := e.beginUpload(t, target)
	if code := uploadErrorCode(t, e.uploadChunk(t, target, uploadID, 5), "upload_chunk_result"); code != "attachment_upload_state_unknown" {
		t.Fatalf("an out-of-order chunk: error code = %s", code)
	}
}

func TestFrameSubmitPromptExpandsAttachmentReferences(t *testing.T) {
	e := newFrameEnv(t)
	target := e.target(t, "pane-1")
	ref := e.uploadAttachment(t, target)

	expectCommandResult(t, e.exchange(t, map[string]any{
		"type": "submit_prompt", "target": target, "text": "Review this file:\nAttachment: " + ref,
	}), "submit_prompt", true, "completed", "")
	prompts := e.backend.called("Prompt")
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Review this file:\\nAttachment: ") ||
		!strings.Contains(prompts[0], "/uploads/objects/") || strings.Contains(prompts[0], "Attachment: "+ref) {
		t.Fatalf("prompts = %v, want the reference replaced by the stored path", prompts)
	}

	const unavailable = "One or more attachments are no longer available for this agent"
	missing := strings.Repeat("a", 32)
	expectCommandResult(t, e.exchange(t, map[string]any{
		"type": "submit_prompt", "target": target, "text": "Look:\nAttachment: " + missing,
	}), "submit_prompt", false, "failed", unavailable)
	expectCommandResult(t, e.exchange(t, map[string]any{
		"type": "submit_prompt", "text": "Look:\nAttachment: " + ref,
	}), "submit_prompt", false, "failed", unavailable)
	if prompts := e.backend.called("Prompt"); len(prompts) != 1 {
		t.Fatalf("prompts = %v, want no prompt for an unresolvable reference", prompts)
	}

	expectCommandResult(t, e.exchange(t, map[string]any{
		"type": "submit_prompt", "target": target, "text": "Attachment: not-a-reference",
	}), "submit_prompt", true, "completed", "")
	if prompts := e.backend.called("Prompt"); len(prompts) != 2 || !strings.Contains(prompts[1], "Attachment: not-a-reference") {
		t.Fatalf("prompts = %v, want text that is not a reference sent as it is", prompts)
	}
}

func TestFrameQRCode(t *testing.T) {
	e := newFrameEnv(t)
	response := e.exchange(t, map[string]any{"type": "qr_code", "text": "https://example.com/pair#secret"})
	expectCommandResult(t, response, "qr_code", true, "completed", "")
	data, _ := response["data"].(map[string]any)
	modules, _ := data["modules"].(string)
	size, _ := data["size"].(float64)
	packed, err := base64.StdEncoding.DecodeString(modules)
	if err != nil || size < 21 || len(packed) != (int(size)*int(size)+7)/8 {
		t.Fatalf("qr data = %#v", data)
	}
	for _, text := range []string{"", "   ", strings.Repeat("x", 4000)} {
		expectCommandResult(t, e.exchange(t, map[string]any{"type": "qr_code", "text": text}),
			"qr_code", false, "failed", "This computer could not encode that QR code")
	}
}

func TestFrameListDirectories(t *testing.T) {
	e := newFrameEnv(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"beta", "Alpha/inner", ".hidden", "gamma"} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.server.home = home
	list := func(fields map[string]any) (string, []string) {
		t.Helper()
		fields["type"] = "list_directories"
		response := e.exchange(t, fields)
		expectCommandResult(t, response, "list_directories", true, "completed", "")
		data, _ := response["data"].(map[string]any)
		current, _ := data["current"].(map[string]any)
		var names []string
		entries, _ := data["directories"].([]any)
		for _, entry := range entries {
			names = append(names, entry.(map[string]any)["name"].(string))
		}
		return current["label"].(string), names
	}

	for name, fields := range map[string]map[string]any{
		"no path":          {},
		"the home":         {"path": home},
		"outside the home": {"path": "/"},
		"a missing path":   {"path": filepath.Join(home, "missing")},
	} {
		if label, names := list(fields); label != "~" || !slices.Equal(names, []string{"Alpha", "beta", "gamma"}) {
			t.Fatalf("%s: label %q, directories %v", name, label, names)
		}
	}
	for name, path := range map[string]string{"a subdirectory": filepath.Join(home, "Alpha"), "a relative path": "Alpha"} {
		if label, names := list(map[string]any{"path": path}); label != "~/Alpha" || !slices.Equal(names, []string{"inner"}) {
			t.Fatalf("%s: label %q, directories %v", name, label, names)
		}
	}
}

func TestFrameGetActivityLimit(t *testing.T) {
	e := newFrameEnv(t)
	e.conn.SetReadLimit(1 << 20)
	for index := range 600 {
		e.server.activityView = append(e.server.activityView, activity.Entry{ID: fmt.Sprintf("entry-%03d", index), Kind: "input", Status: "sent", Summary: "sent"})
	}
	fetch := func(fields map[string]any) []string {
		t.Helper()
		fields["type"] = "get_activity"
		response := e.exchange(t, fields)
		if response["type"] != "activity_history" {
			t.Fatalf("response = %#v", response)
		}
		var ids []string
		activities, _ := response["activities"].([]any)
		for _, entry := range activities {
			ids = append(ids, entry.(map[string]any)["id"].(string))
		}
		return ids
	}
	for name, test := range map[string]struct {
		fields map[string]any
		count  int
	}{
		"absent":        {map[string]any{}, 500},
		"a small limit": {map[string]any{"limit": 3}, 3},
		"one":           {map[string]any{"limit": 1}, 1},
		"the maximum":   {map[string]any{"limit": 500}, 500},
		"zero":          {map[string]any{"limit": 0}, 500},
		"negative":      {map[string]any{"limit": -4}, 500},
		"above the max": {map[string]any{"limit": 501}, 500},
	} {
		ids := fetch(test.fields)
		if len(ids) != test.count || ids[len(ids)-1] != "entry-599" || ids[0] != fmt.Sprintf("entry-%03d", 600-test.count) {
			t.Fatalf("%s: got %d entries from %v to %v, want the last %d", name, len(ids), ids[:1], ids[len(ids)-1:], test.count)
		}
	}
	response := e.exchange(t, map[string]any{"type": "get_activity", "limit": 1.5})
	if apiError := apiErrorOf(t, response); apiError["code"] != protocol.ErrorInvalidRequest {
		t.Fatalf("a fractional limit: error = %#v, want invalid_request", apiError)
	}
}

func TestFrameClearActivitiesEchoesTheRequestID(t *testing.T) {
	e := newFrameEnv(t)
	response := e.exchange(t, map[string]any{"type": "clear_activities", "request_id": "clear-7"})
	if response["type"] != "command_result" || response["action"] != "clear_activities" || response["ok"] != true || response["request_id"] != "clear-7" {
		t.Fatalf("response = %#v", response)
	}
}

func TestFrameListSlashCommandsNeedsAKnownPane(t *testing.T) {
	e := newFrameEnv(t)
	expectCommandResult(t, e.exchange(t, map[string]any{"type": "list_slash_commands", "pane_id": nil}), "list_slash_commands", false, "failed", "Agent is required")
	expectCommandResult(t, e.exchange(t, map[string]any{"type": "list_slash_commands", "pane_id": "missing"}), "list_slash_commands", false, "failed", "Agent pane not found")
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "frame")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newPiFrameEnv(t *testing.T) *frameEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv(agentroots.PiListEnv, "")
	e := newFrameEnv(t, &coordinator.AgentState{
		PaneID: "pane-1", Agent: "pi", TerminalID: "terminal-1", Generation: 1, SessionID: "agent-session-1",
		ServerSessionID: protocol.PrimaryServerSessionID, Status: "blocked", AttentionKind: "unknown",
	})
	path := filepath.Join(shortSocketDir(t), "herdr.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	e.server.cfg.SocketPath = path
	return e
}

// Pi actions are controller-only and a test connection cannot authenticate,
// so these frames enter at the Pi handlers, after the gate in the message handler.
// The audit attempt is written first, as the message handler does for audited writes.
func (e *frameEnv) piPromptFrame(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	frame := frameWithDefaults(fields)
	inbound, err := protocol.DecodeMap(frame)
	if err != nil {
		t.Fatal(err)
	}
	e.recordAttempt(frame, inbound)
	result := e.server.handlePiPrompt(t.Context(), e.client, inbound, frame, func() {})
	e.server.hub.Send(e.client, coordinator.CommandResultMessage(result))
	return readFrame(t, e.conn)
}

func (e *frameEnv) piIntegrationFrame(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	frame := map[string]any{"request_id": "req-1", "protocol": protocol.Version}
	for key, value := range fields {
		frame[key] = value
	}
	inbound, err := protocol.DecodeMap(frame)
	if err != nil {
		t.Fatal(err)
	}
	e.recordAttempt(frame, inbound)
	e.server.handlePiIntegration(t.Context(), e.client, inbound, frame)
	return readFrame(t, e.conn)
}

func (e *frameEnv) recordAttempt(frame map[string]any, inbound protocol.Inbound) {
	if isAuditedWrite(inbound.Type) {
		e.server.recordWriteAudit(e.client, frame, nil)
	}
}

func (e *frameEnv) forgetBackendCalls() {
	e.backend.mu.Lock()
	defer e.backend.mu.Unlock()
	e.backend.calls = nil
}

func TestFramePiPromptAcceptsOnlyPromptKeys(t *testing.T) {
	const unavailable = "Pi prompt capability is unavailable for this target"
	e := newPiFrameEnv(t)
	target := e.target(t, "pane-1")
	var ref protocol.TargetRef
	decodeInto(t, target, &ref)
	targetKey, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	lease := map[string]any{
		"instance": "relay", "pane": "pane-1", "session": "agent-session-1", "pid": 4242, "incarnation": "incarnation-1",
		"connection": e.client.ID(), "target_key": string(targetKey), "token": "token-1", "expires_at": time.Now().Add(time.Minute).Unix(),
	}

	for name, test := range map[string]struct {
		fields   map[string]any
		reaching string
	}{
		"capability":      {map[string]any{"type": "get_pi_prompt_capability", "target": target}, "PaneProcessInfoFresh"},
		"capability, ids": {map[string]any{"type": "get_pi_prompt_capability", "target": target, "client_id": "device:relay", "server_session_id": protocol.PrimaryServerSessionID}, "PaneProcessInfoFresh"},
		"send":            {map[string]any{"type": "send_pi_prompt_unconfirmed", "target": target, "text": "hello", "pi_prompt_lease": lease}, "PaneProcessInfoFresh"},
		"send, client id": {map[string]any{"type": "send_pi_prompt_unconfirmed", "target": target, "text": "hello", "pi_prompt_lease": lease, "client_id": "device:relay"}, "PaneProcessInfoFresh"},
	} {
		t.Run(name+" is accepted", func(t *testing.T) {
			e.forgetBackendCalls()
			expectCommandResult(t, e.piPromptFrame(t, test.fields), test.fields["type"].(string), false, "not_started", unavailable)
			if calls := e.backend.called(test.reaching); len(calls) != 1 {
				t.Fatalf("%s calls = %v, want the request to reach the Pi process lookup", test.reaching, e.backend.calls)
			}
		})
	}
	for _, extra := range []map[string]any{
		{"keys": []string{"Enter"}}, {"prompt": "x"}, {"secret": true}, {"attachments": []string{"x"}}, {"label": "x"}, {"workspace_id": "w"},
		{"no_echo": true}, {"images": []string{"x"}},
	} {
		for _, base := range []map[string]any{
			{"type": "get_pi_prompt_capability", "target": target},
			{"type": "send_pi_prompt_unconfirmed", "target": target, "text": "hello", "pi_prompt_lease": lease},
		} {
			t.Run(fmt.Sprint(base["type"], " with ", extra), func(t *testing.T) {
				e.forgetBackendCalls()
				fields := map[string]any{}
				maps.Copy(fields, base)
				maps.Copy(fields, extra)
				expectCommandResult(t, e.piPromptFrame(t, fields), base["type"].(string), false, "not_started", unavailable)
				if calls := e.backend.called("PaneProcessInfoFresh"); len(calls) != 0 {
					t.Fatalf("a disallowed key reached the Pi process lookup: %v", calls)
				}
			})
		}
	}
}

func TestFramePiIntegrationAcceptsOnlyItsKeys(t *testing.T) {
	const mismatch = "Pi integration request does not match this relay"
	e := newPiFrameEnv(t)
	instance := e.server.piIntegration.Instance()
	set := func(fields map[string]any) map[string]any {
		frame := map[string]any{"type": "set_pi_integration", "instance": instance, "enabled": false}
		maps.Copy(frame, fields)
		return e.piIntegrationFrame(t, frame)
	}

	for name, extra := range map[string]map[string]any{"only its keys": {}, "a client id": {"client_id": "device:relay"}} {
		response := set(extra)
		if response["type"] != "command_result" || response["action"] != "set_pi_integration" || response["ok"] != true || response["phase"] != "completed" {
			t.Fatalf("%s: response = %#v, want the integration state", name, response)
		}
		if data, _ := response["data"].(map[string]any); data["instance"] != instance || data["enabled"] != false {
			t.Fatalf("%s: state = %#v", name, response["data"])
		}
	}
	for _, extra := range []map[string]any{
		{"path": "/tmp/pi"}, {"profiles": []string{"x"}}, {"target": map[string]any{"pane_id": "pane-1"}},
		{"pane_id": "pane-1"}, {"server_session_id": protocol.PrimaryServerSessionID}, {"text": "remote"},
	} {
		expectCommandResult(t, set(extra), "set_pi_integration", false, "failed", mismatch)
	}
	for name, instance := range map[string]string{"another relay": "other-relay", "no instance": ""} {
		if response := set(map[string]any{"instance": instance}); response["ok"] != false || response["error"] != mismatch {
			t.Fatalf("%s: response = %#v, want %q", name, response, mismatch)
		}
	}

	for name, enabled := range map[string]any{"null": nil, "a string": "true", "a number": 1} {
		if response := set(map[string]any{"enabled": enabled}); response["ok"] != false || response["error"] != mismatch {
			t.Fatalf("enabled as %s: response = %#v, want %q", name, response, mismatch)
		}
	}

	if response := e.piIntegrationFrame(t, map[string]any{"type": "get_pi_integration"}); response["ok"] != true || response["phase"] != "completed" {
		t.Fatalf("get_pi_integration = %#v", response)
	}
}

func newWebRTCFrameEnv(t *testing.T) *frameEnv {
	t.Helper()
	e := newFrameEnv(t)
	manager, err := webrtclink.New(webrtclink.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxSessions: 2,
		Serve: func(context.Context, transport.FrameConn) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	e.server.hybrid = &hybridTransport{webrtc: manager, signals: make(map[string]*signalBudget)}
	return e
}

func browserOffer(t *testing.T) string {
	t.Helper()
	peer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := peer.CreateDataChannel("herdr-dc-v1", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	return offer.SDP
}

func TestFrameWebRTCOfferNeedsAnSDP(t *testing.T) {
	for name, fields := range map[string]map[string]any{"absent": {}, "empty": {"sdp": ""}, "null": {"sdp": nil}} {
		t.Run(name, func(t *testing.T) {
			e := newWebRTCFrameEnv(t)
			fields["type"] = "webrtc_offer"
			expectCommandResult(t, e.exchange(t, fields), "webrtc_offer", false, "failed", "sdp is required")
		})
	}
}

func TestFrameWebRTCSignalling(t *testing.T) {
	e := newWebRTCFrameEnv(t)
	offer := browserOffer(t)
	if !strings.HasSuffix(offer, "\r\n") {
		t.Fatalf("offer does not end with its line terminator: %q", offer[len(offer)-10:])
	}

	answer := e.exchange(t, map[string]any{"type": "webrtc_offer", "sdp": offer})
	if sdp, _ := answer["sdp"].(string); answer["type"] != "webrtc_answer" || answer["request_id"] != "req-1" || !strings.HasPrefix(sdp, "v=0") {
		t.Fatalf("answer = %#v, want an answer to the offer as sent", answer)
	}

	const candidate = "candidate:1 1 udp 2130706431 192.0.2.1 4444 typ host"
	for _, fields := range map[string]map[string]any{
		"a candidate":                  {"candidate": candidate, "sdp_mid": "0", "sdp_mline_index": 0},
		"a candidate on a later line":  {"candidate": candidate, "sdp_mid": "1", "sdp_mline_index": 1},
		"a candidate without a mid":    {"candidate": candidate, "sdp_mid": nil, "sdp_mline_index": nil},
		"a candidate with only a body": {"candidate": candidate},
		"no candidate":                 {"sdp_mid": "0", "sdp_mline_index": 0},
		"an empty candidate":           {"candidate": "", "sdp_mid": "0", "sdp_mline_index": 0},
	} {
		fields["type"] = "webrtc_ice"
		sendFrame(t, e.conn, frameWithDefaults(fields))
	}
	if replies := e.settle(t); len(replies) != 0 {
		t.Fatalf("ICE candidates got replies: %v", replies)
	}

	closed := e.exchange(t, map[string]any{"type": "webrtc_close"})
	if closed["type"] != "webrtc_closed" || closed["request_id"] != "req-1" {
		t.Fatalf("close reply = %#v", closed)
	}
}

func TestFrameWebRTCOfferLosesItsTerminatorOnlyWhenTrimmed(t *testing.T) {
	e := newWebRTCFrameEnv(t)
	trimmed := strings.TrimRight(browserOffer(t), "\r\n")
	response := e.exchange(t, map[string]any{"type": "webrtc_offer", "sdp": trimmed})
	if response["type"] != "command_result" || response["ok"] != false {
		t.Fatalf("an offer without its final terminator got %#v, want a refusal", response)
	}
}

type logCapture struct {
	mu   sync.Mutex
	text strings.Builder
}

func (c *logCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.Write(data)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

func TestFrameWebRTCCandidateReachesTheICEHandlerVerbatim(t *testing.T) {
	e := newWebRTCFrameEnv(t)
	logs := &logCapture{}
	e.server.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if answer := e.exchange(t, map[string]any{"type": "webrtc_offer", "sdp": browserOffer(t)}); answer["type"] != "webrtc_answer" {
		t.Fatalf("answer = %#v", answer)
	}

	sendFrame(t, e.conn, frameWithDefaults(map[string]any{
		"type": "webrtc_ice", "candidate": "candidate:1 1 udp 2130706431 192.0.2.1 4444 typ host", "sdp_mid": "0", "sdp_mline_index": 3,
	}))
	e.settle(t)
	if strings.Contains(logs.String(), "webrtc candidate rejected") {
		t.Fatalf("a valid candidate was refused: %s", logs.String())
	}

	sendFrame(t, e.conn, frameWithDefaults(map[string]any{"type": "webrtc_ice", "candidate": "not a candidate", "sdp_mid": "0", "sdp_mline_index": 0}))
	e.settle(t)
	if !strings.Contains(logs.String(), "webrtc candidate rejected") {
		t.Fatalf("a candidate that is not valid never reached the ICE handler: %s", logs.String())
	}
}
