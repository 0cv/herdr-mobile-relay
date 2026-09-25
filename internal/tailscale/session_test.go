package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const sessionTestID = "watch-generated-session-id"

const sessionTestStatus = `{"Version":"` + localAPITestVersion + `","BackendState":"Running","Self":{"ID":"node-1","UserID":123,"DNSName":"relay.tailnet.ts.net.","CapMap":{"https":null}},"CurrentTailnet":{"Name":"example-account","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":true},"CertDomains":["relay.tailnet.ts.net"],"User":{"123":{"ID":123,"LoginName":"user@example.invalid","DisplayName":"Example","ProfilePicURL":""}}}`

type sessionDaemon struct {
	mu sync.Mutex

	status                         []byte
	config                         []byte
	revision                       uint64
	omitETag                       bool
	changeIdentityAtWatch          bool
	changeIdentityAfterWatchConfig bool
	changeConfigAtWatch            bool
	watcherDeleteMapRemovalGap     bool
	conflictRegistration           bool
	conflictRegistrationSameKey    bool
	conflictCleanup                bool
	cleanupConflictsRemaining      int
	dropRegistrationAck            bool
	dropDeleteAck                  bool
	dropRegistrationWithoutApply   bool
	stallRegistration              bool
	lateApply                      chan struct{}
	lateApplied                    chan struct{}
	replaceOnAbsentRead            bool
	cleanupOnLocalClose            bool
	delayedCleanupOnLocalClose     bool

	watch         *sessionFakeStream
	watchCount    int
	writeBodies   [][]byte
	writeTags     []string
	writeStatuses []int
	postCount     int
}

func newSessionDaemon() *sessionDaemon {
	return &sessionDaemon{status: []byte(sessionTestStatus), config: []byte("null"), revision: 1}
}

func (d *sessionDaemon) etagLocked() string { return fmt.Sprintf("%064x", d.revision) }

func (d *sessionDaemon) RoundTrip(request *http.Request) (*http.Response, error) {
	d.mu.Lock()
	switch request.URL.Path {
	case localAPIStatusPath:
		body := append([]byte(nil), d.status...)
		d.mu.Unlock()
		return localAPIResponse(request, http.StatusOK, localAPIJSONHeader(), testBody(string(body))), nil
	case localAPIServeConfigPath:
		if request.Method == http.MethodGet {
			header := localAPIJSONHeader()
			if !d.omitETag {
				header.Set("ETag", d.etagLocked())
			}
			body := append([]byte(nil), d.config...)
			if d.changeIdentityAfterWatchConfig && d.watch != nil {
				d.changeIdentityAfterWatchConfig = false
				d.status = bytes.Replace(d.status, []byte(`"ID":"node-1"`), []byte(`"ID":"node-2"`), 1)
			}
			if d.replaceOnAbsentRead {
				if _, present, _ := foregroundEntry(body, sessionTestID); !present {
					d.replaceOnAbsentRead = false
					d.config = addSameKeyReplacement(body)
					d.revision++
				}
			}
			d.mu.Unlock()
			return localAPIResponse(request, http.StatusOK, header, io.NopCloser(bytes.NewReader(body))), nil
		}
		return d.roundTripWriteLocked(request)
	case localAPIWatchPath:
		if d.changeIdentityAtWatch {
			d.status = bytes.Replace(d.status, []byte(`"ID":"node-1"`), []byte(`"ID":"node-2"`), 1)
		}
		if d.changeConfigAtWatch {
			d.config = []byte(`{"Services":{"svc:foreign":{"Tun":true}}}`)
			d.revision++
		}
		d.watchCount++
		stream := newSessionFakeStream(sessionTestID, func() { d.cleanupWatchSession(sessionTestID) })
		d.watch = stream
		d.mu.Unlock()
		header := localAPIJSONHeader()
		return localAPIResponse(request, http.StatusOK, header, stream), nil
	default:
		d.mu.Unlock()
		return nil, errors.New("unexpected session LocalAPI path")
	}
}

func (d *sessionDaemon) roundTripWriteLocked(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, MaxOutputBytes+1))
	if err != nil || len(body) > MaxOutputBytes {
		d.mu.Unlock()
		return nil, errors.New("test request body failure")
	}
	d.postCount++
	d.writeBodies = append(d.writeBodies, append([]byte(nil), body...))
	d.writeTags = append(d.writeTags, request.Header.Get("If-Match"))
	if request.GetBody != nil || request.Header.Get("Idempotency-Key") != "" {
		d.mu.Unlock()
		return nil, errors.New("test observed replayable or idempotent POST")
	}
	if request.Header.Get("If-Match") != d.etagLocked() {
		d.writeStatuses = append(d.writeStatuses, http.StatusPreconditionFailed)
		d.mu.Unlock()
		return d.sessionConflict(request), nil
	}

	var next map[string]json.RawMessage
	if json.Unmarshal(body, &next) != nil || next == nil {
		d.mu.Unlock()
		return nil, errors.New("test config POST is not an object")
	}
	sessionValue, hasSession := sessionEntry(next, sessionTestID)
	_, currentlyPresent, _ := foregroundEntry(d.config, sessionTestID)
	registration := hasSession && !currentlyPresent
	if registration && d.conflictRegistration {
		d.conflictRegistration = false
		if d.conflictRegistrationSameKey {
			d.config = addSameKeyReplacement([]byte(`{}`))
			d.conflictRegistrationSameKey = false
		} else {
			d.config = []byte(`{"Services":{"svc:foreign":{"Tun":true}}}`)
		}
		d.revision++
		d.writeStatuses = append(d.writeStatuses, http.StatusPreconditionFailed)
		d.mu.Unlock()
		return d.sessionConflict(request), nil
	}
	if !registration && currentlyPresent && (d.conflictCleanup || d.cleanupConflictsRemaining > 0) {
		d.conflictCleanup = false
		if d.cleanupConflictsRemaining > 0 {
			d.cleanupConflictsRemaining--
		}
		d.config = addForeignConfig(d.config)
		d.revision++
		d.writeStatuses = append(d.writeStatuses, http.StatusPreconditionFailed)
		d.mu.Unlock()
		return d.sessionConflict(request), nil
	}
	if registration && (d.dropRegistrationWithoutApply || d.stallRegistration) {
		late := d.lateApply
		applied := d.lateApplied
		value := append([]byte(nil), body...)
		waitForCancel := d.stallRegistration
		d.writeStatuses = append(d.writeStatuses, 0)
		d.mu.Unlock()
		if late != nil {
			go func() {
				if waitForCancel {
					<-request.Context().Done()
				}
				<-late
				d.mu.Lock()
				d.config = value
				d.revision++
				d.mu.Unlock()
				if applied != nil {
					close(applied)
				}
			}()
		}
		if waitForCancel {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return nil, errors.New("synthetic lost registration response")
	}
	if !registration && !currentlyPresent {
		d.writeStatuses = append(d.writeStatuses, http.StatusOK)
		d.mu.Unlock()
		return localAPIResponse(request, http.StatusOK, http.Header{"Tailscale-Version": {localAPITestVersion}}, testBody("")), nil
	}
	if registration && d.watcherDeleteMapRemovalGap {
		d.watcherDeleteMapRemovalGap = false
		stream := d.watch
		// Model DeleteForegroundSession first while the watch is still in the
		// server watcher map, then a conditional SetServeConfig in the gap, and
		// only then deliver EOF after map removal.
		d.deleteOwnedSessionLocked(sessionTestID)
		d.config = append([]byte(nil), body...)
		d.revision++
		d.writeStatuses = append(d.writeStatuses, http.StatusOK)
		d.mu.Unlock()
		if stream != nil {
			stream.lose()
		}
		return localAPIResponse(request, http.StatusOK, http.Header{"Tailscale-Version": {localAPITestVersion}}, testBody("")), nil
	}
	d.config = append([]byte(nil), body...)
	d.revision++
	if registration && d.dropRegistrationAck || !registration && d.dropDeleteAck {
		d.writeStatuses = append(d.writeStatuses, 0)
		d.mu.Unlock()
		return nil, errors.New("synthetic lost acknowledgement")
	}
	d.writeStatuses = append(d.writeStatuses, http.StatusOK)
	d.mu.Unlock()
	_ = sessionValue
	return localAPIResponse(request, http.StatusOK, http.Header{"Tailscale-Version": {localAPITestVersion}}, testBody("")), nil
}

func (d *sessionDaemon) sessionConflict(request *http.Request) *http.Response {
	header := http.Header{"Tailscale-Version": {localAPITestVersion}, "Content-Type": {"text/plain; charset=utf-8"}}
	return localAPIResponse(request, http.StatusPreconditionFailed, header, testBody("etag mismatch\n"))
}

func sessionEntry(root map[string]json.RawMessage, session string) (json.RawMessage, bool) {
	var sessions map[string]json.RawMessage
	if json.Unmarshal(root["Foreground"], &sessions) != nil {
		return nil, false
	}
	value, ok := sessions[session]
	return value, ok
}

func addSameKeyReplacement(data []byte) []byte {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(data, &root)
	if root == nil {
		root = make(map[string]json.RawMessage)
	}
	var foreground map[string]json.RawMessage
	_ = json.Unmarshal(root["Foreground"], &foreground)
	if foreground == nil {
		foreground = make(map[string]json.RawMessage)
	}
	foreground[sessionTestID] = json.RawMessage(`{"TCP":{"443":{"HTTPS":true}},"Web":{"relay.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}}},"Services":{"svc:repurposed":{"Tun":true}}}`)
	root["Foreground"], _ = json.Marshal(foreground)
	result, _ := json.Marshal(root)
	return result
}

func addForeignConfig(data []byte) []byte {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(data, &root)
	if root == nil {
		root = make(map[string]json.RawMessage)
	}
	root["Services"] = json.RawMessage(`{"svc:foreign":{"Tun":true}}`)
	root["AllowFunnel"] = json.RawMessage(`{"foreign.tailnet.ts.net:443":false}`)
	var foreground map[string]json.RawMessage
	_ = json.Unmarshal(root["Foreground"], &foreground)
	if foreground == nil {
		foreground = make(map[string]json.RawMessage)
	}
	foreground["independent-session"] = json.RawMessage(configRouteEntry)
	root["Foreground"], _ = json.Marshal(foreground)
	result, _ := json.Marshal(root)
	return result
}

func (d *sessionDaemon) setStatus(body []byte) {
	d.mu.Lock()
	d.status = append([]byte(nil), body...)
	d.mu.Unlock()
}

func (d *sessionDaemon) setConfig(body []byte) {
	d.mu.Lock()
	d.config = append([]byte(nil), body...)
	d.revision++
	d.mu.Unlock()
}

func (d *sessionDaemon) currentConfig() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.config...)
}

func (d *sessionDaemon) counts() (watches, posts int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.watchCount, d.postCount
}

func (d *sessionDaemon) loseWatch(session string) {
	d.mu.Lock()
	stream := d.watch
	d.deleteOwnedSessionLocked(session)
	d.mu.Unlock()
	if stream != nil {
		stream.lose()
	}
}

func (d *sessionDaemon) deleteOwnedSession(session string) {
	d.mu.Lock()
	d.deleteOwnedSessionLocked(session)
	d.mu.Unlock()
}

func (d *sessionDaemon) cleanupWatchSession(session string) {
	d.mu.Lock()
	delayed := d.delayedCleanupOnLocalClose
	cleanup := d.cleanupOnLocalClose
	d.mu.Unlock()
	if delayed {
		go func() {
			time.Sleep(40 * time.Millisecond)
			d.deleteOwnedSession(session)
		}()
		return
	}
	if cleanup {
		d.deleteOwnedSession(session)
	}
}

func (d *sessionDaemon) deleteOwnedSessionLocked(session string) {
	var root map[string]json.RawMessage
	_ = json.Unmarshal(d.config, &root)
	if root == nil {
		return
	}
	var sessions map[string]json.RawMessage
	_ = json.Unmarshal(root["Foreground"], &sessions)
	delete(sessions, session)
	root["Foreground"], _ = json.Marshal(sessions)
	d.config, _ = json.Marshal(root)
	d.revision++
}

type sessionFakeStream struct {
	reader  *io.PipeReader
	writer  *io.PipeWriter
	once    sync.Once
	onClose func()
}

func newSessionFakeStream(session string, onClose func()) *sessionFakeStream {
	reader, writer := io.Pipe()
	stream := &sessionFakeStream{reader: reader, writer: writer, onClose: onClose}
	go func() { _, _ = io.WriteString(writer, watchNotification(localAPITestVersion, session)+"\n") }()
	return stream
}

func (s *sessionFakeStream) Read(buffer []byte) (int, error) { return s.reader.Read(buffer) }
func (s *sessionFakeStream) Close() error {
	s.once.Do(func() {
		_ = s.reader.Close()
		_ = s.writer.Close()
		if s.onClose != nil {
			s.onClose()
		}
	})
	return nil
}
func (s *sessionFakeStream) lose() { _ = s.writer.Close() }

func newSessionTestAuthority(d *sessionDaemon) *SessionAuthority {
	api := newTestLocalAPI(localAPITestRoundTripper(d.RoundTrip))
	authority, err := newSessionAuthority(api, 443, 8375)
	if err != nil {
		panic(err)
	}
	return authority
}

func prepareAndActivate(t *testing.T, d *sessionDaemon) *SessionAuthority {
	t.Helper()
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := authority.Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return authority
}

func TestSessionHTTPSCapabilityComesFromPinnedSelfCapMap(t *testing.T) {
	withHTTPS := strings.Replace(sourceStatus, `"DNSName":"relay.tailnet.ts.net."`, `"DNSName":"relay.tailnet.ts.net.","CapMap":{"https":null}`, 1)
	if _, err := ParseStatus([]byte(withHTTPS)); err != nil {
		t.Fatalf("status with pinned CapMap rejected: %v", err)
	}
	capable, err := pinnedHTTPSCapability([]byte(withHTTPS))
	if err != nil || !capable {
		t.Fatalf("pinned CapMap HTTPS capability = %t, err=%v", capable, err)
	}
	deprecatedOnly := strings.Replace(sourceStatus, `"DNSName":"relay.tailnet.ts.net."`, `"DNSName":"relay.tailnet.ts.net.","Capabilities":["https"]`, 1)
	capable, err = pinnedHTTPSCapability([]byte(deprecatedOnly))
	if err != nil || capable {
		t.Fatalf("deprecated capability field was treated as HTTPS readiness: %t, err=%v", capable, err)
	}
	for _, raw := range []string{`"CapMap":"https"`, `"CapMap":{"https":true}`, `"CapMap":{"https":null,"other":true}`} {
		invalid := strings.Replace(sourceStatus, `"DNSName":"relay.tailnet.ts.net."`, `"DNSName":"relay.tailnet.ts.net.",`+raw, 1)
		if _, err := pinnedHTTPSCapability([]byte(invalid)); err == nil {
			t.Errorf("invalid pinned capability schema accepted: %s", raw)
		}
	}
}

func TestSessionAuthorityZeroValueFailsClosed(t *testing.T) {
	var authority SessionAuthority
	if err := authority.Prepare(context.Background()); err == nil {
		t.Fatal("zero-value authority prepared without the production transport")
	}
	if err := authority.Activate(context.Background()); err == nil {
		t.Fatal("zero-value authority activated")
	}
	if err := authority.Validate(context.Background()); err == nil {
		t.Fatal("zero-value authority validated")
	}
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("zero-value authority retired/adopted state")
	}
	select {
	case <-authority.Invalidation():
	default:
		t.Fatal("zero-value authority did not expose fail-closed invalidation")
	}
	if status := authority.Status(); !status.RemoteWatchRetirementUnknown || status.Active || status.RouteValidated {
		t.Fatalf("zero-value status = %+v", status)
	}
}

func TestSessionAuthorityValidatedRouteSerializesCommitAgainstWatchLoss(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	authority.mu.Lock()
	watch := authority.watch
	authority.mu.Unlock()
	if watch == nil {
		t.Fatal("active SessionAuthority has no retained watch")
	}
	admissionStarted := make(chan struct{})
	releaseAdmission := make(chan struct{})
	operationDone := make(chan error, 1)
	committed := false
	go func() {
		operationDone <- authority.WithValidatedRoute(context.Background(), func() error {
			close(admissionStarted)
			<-releaseAdmission
			return nil
		}, func() error {
			committed = true
			return nil
		})
	}()
	select {
	case <-admissionStarted:
	case <-time.After(time.Second):
		t.Fatal("validated admission callback did not start")
	}
	d.loseWatch(sessionTestID)
	select {
	case <-watch.done():
	case <-time.After(time.Second):
		close(releaseAdmission)
		t.Fatal("local watch did not observe injected EOF")
	}
	close(releaseAdmission)
	select {
	case err := <-operationDone:
		if err == nil {
			t.Fatal("owner commit proceeded after watch loss during final admission")
		}
	case <-time.After(time.Second):
		t.Fatal("validated owner operation did not finish after watch loss")
	}
	if committed {
		t.Fatal("commit callback ran after the final live-watch check failed")
	}
	waitForSessionStatus(t, authority, func(status AuthorityStatus) bool { return status.Invalidated && status.LocalWatchClosed })
}

func TestSessionAuthorityRegistersAndValidatesOnlyItsLiveWatcher(t *testing.T) {
	d := newSessionDaemon()
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := authority.Status(); !status.Prepared || status.RouteValidated || status.Active {
		t.Fatalf("prepared owner exposed authority: %+v", status)
	}
	if err := authority.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := authority.Status(); !status.Active || status.RouteValidated || status.RegistrationOutcome != "settled-success" {
		t.Fatalf("activation state before validation = %+v", status)
	}
	if err := authority.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if status := authority.Status(); !status.RouteValidated || !status.Active || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("validated owner state = %+v", status)
	}
	select {
	case <-authority.Invalidation():
		t.Fatal("valid live authority signaled invalidation")
	default:
	}
	watches, posts := d.counts()
	if watches != 1 || posts != 1 {
		t.Fatalf("owned LocalAPI watch/registration counts = %d/%d", watches, posts)
	}
	d.mu.Lock()
	body := append([]byte(nil), d.writeBodies[0]...)
	tag := d.writeTags[0]
	d.mu.Unlock()
	if !validServeETag(tag) {
		t.Fatalf("conditional registration ETag = %q", tag)
	}
	root, _, err := parseFullServeConfig(body)
	if err != nil {
		t.Fatalf("registration body schema: %v", err)
	}
	entry, ok, err := foregroundEntry(body, sessionTestID)
	if err != nil || !ok || !sameJSON(entry, authority.routeJSON) || len(root) != 1 {
		t.Fatalf("registration did not use watch's exact initial ID/config: ok=%t err=%v", ok, err)
	}
	if strings.Contains(authority.String(), sessionTestID) {
		t.Fatal("caller-facing owner diagnostics disclosed the watch ID")
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("test owner cleanup: %v", err)
	}
}

func TestSessionAuthorityRejectsIdentityAndCapabilityDriftBeforeWrite(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*sessionDaemon)
	}{
		{name: "identity before watch", mutate: func(d *sessionDaemon) {
			d.setStatus(bytes.Replace([]byte(sessionTestStatus), []byte(`"ID":"node-1"`), []byte(`"ID":"node-2"`), 1))
		}},
		{name: "capability removed before watch", mutate: func(d *sessionDaemon) {
			d.setStatus([]byte(strings.Replace(sessionTestStatus, `,"CapMap":{"https":null}`, ``, 1)))
		}},
		{name: "identity changes after watch", mutate: func(d *sessionDaemon) { d.changeIdentityAtWatch = true }},
		{name: "identity changes during final config read", mutate: func(d *sessionDaemon) { d.changeIdentityAfterWatchConfig = true }},
		{name: "version changes before watch", mutate: func(d *sessionDaemon) {
			d.setStatus([]byte(strings.Replace(sessionTestStatus, localAPITestVersion, "1.102.4-foreign", 1)))
		}},
		{name: "foreign config arrives after watch", mutate: func(d *sessionDaemon) { d.changeConfigAtWatch = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := newSessionDaemon()
			authority := newSessionTestAuthority(d)
			if err := authority.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			test.mutate(d)
			if err := authority.Activate(context.Background()); err == nil {
				t.Fatal("identity/capability drift admitted")
			}
			watches, posts := d.counts()
			if posts != 0 {
				t.Fatalf("drift dispatched %d writes", posts)
			}
			if watches == 1 {
				d.loseWatch(sessionTestID)
				waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
			}
		})
	}
}

func TestSessionAuthorityPreparedOriginAndSafePreActivationRetirement(t *testing.T) {
	d := newSessionDaemon()
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if origin, ok := authority.Origin(); !ok || origin != "https://relay.tailnet.ts.net" {
		t.Fatalf("prepared origin = %q, %t", origin, ok)
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("retire before activation: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("safe pre-activation retirement status = %+v", status)
	}
	watches, posts := d.counts()
	if watches != 0 || posts != 0 {
		t.Fatalf("pre-activation retirement opened %d watches and issued %d writes", watches, posts)
	}
}

func TestSessionAuthorityRequiresEmptyConfigAndETagBeforeWatchOrWrite(t *testing.T) {
	d := newSessionDaemon()
	d.omitETag = true
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err == nil {
		t.Fatal("missing ETag accepted during read-only prepare")
	}
	watches, posts := d.counts()
	if watches != 0 || posts != 0 {
		t.Fatalf("missing ETag opened watch/wrote config: %d/%d", watches, posts)
	}

	observed := newSessionDaemon()
	observed.config = []byte(`{"Foreground":{"observed-session":` + configRouteEntry + `}}`)
	withoutHandle := newSessionTestAuthority(observed)
	if err := withoutHandle.Prepare(context.Background()); err == nil {
		t.Fatal("observed matching route admitted as a new invocation")
	}
	if err := withoutHandle.Validate(context.Background()); err == nil {
		t.Fatal("route observation without a held watch validated")
	}
	if err := withoutHandle.Retire(context.Background()); err == nil {
		t.Fatal("route observation without a held watch retired/adopted")
	}
	_, posts = observed.counts()
	if posts != 0 {
		t.Fatalf("observed route caused %d writes", posts)
	}
}

func TestSessionAuthorityNoWrite412PreservesForeignConfigAndCanClear(t *testing.T) {
	d := newSessionDaemon()
	d.conflictRegistration = true
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Activate(context.Background()); !errors.Is(err, errSessionNoWrite) {
		t.Fatalf("registration conflict error = %v", err)
	}
	if status := authority.Status(); status.RegistrationOutcome != "settled-no-write" || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("settled 412 state = %+v", status)
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("Retire after settled no-write: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("no-write retirement facts = %+v", status)
	}
	config := d.currentConfig()
	if _, _, err := parseFullServeConfig(config); err != nil {
		t.Fatalf("foreign config did not survive: %v", err)
	}
	watches, posts := d.counts()
	if watches != 1 || posts != 1 {
		t.Fatalf("watch/write retry count = %d/%d", watches, posts)
	}
}

func TestSessionAuthoritySettledNoWriteWithSameKeyConflictRefusesClose(t *testing.T) {
	d := newSessionDaemon()
	d.conflictRegistration = true
	d.conflictRegistrationSameKey = true
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Activate(context.Background()); !errors.Is(err, errSessionNoWrite) {
		t.Fatalf("registration conflict = %v", err)
	}
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("same-key entry after settled no-write was adopted/deleted")
	}
	status := authority.Status()
	if !status.Quarantined || status.RouteCleared || status.LocalWatchClosed || status.RegistrationOutcome != "settled-no-write" {
		t.Fatalf("same-key settled-no-write state = %+v", status)
	}
	watches, posts := d.counts()
	if watches != 1 || posts != 1 {
		t.Fatalf("same-key settled-no-write wrote or closed: watches=%d posts=%d", watches, posts)
	}
	d.loseWatch(sessionTestID)
	waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
}

func TestSessionAuthorityCleanupRebasesAndPreservesRecognizedForeignFields(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.mu.Lock()
	d.conflictCleanup = true
	d.mu.Unlock()
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("Retire with one concurrent ETag conflict: %v", err)
	}
	config := d.currentConfig()
	root, _, err := parseFullServeConfig(config)
	if err != nil {
		t.Fatalf("retired full config: %v", err)
	}
	for _, field := range []string{"Services", "AllowFunnel", "Foreground"} {
		if _, ok := root[field]; !ok {
			t.Errorf("recognized concurrent field %s was dropped", field)
		}
	}
	entry, present, err := foregroundEntry(config, sessionTestID)
	if err != nil || present || len(entry) != 0 {
		t.Fatalf("owned foreground entry remains: present=%t err=%v", present, err)
	}
	var rootFields map[string]json.RawMessage
	_ = json.Unmarshal(config, &rootFields)
	if !sameJSON(rootFields["Services"], json.RawMessage(`{"svc:foreign":{"Tun":true}}`)) {
		t.Fatal("foreign service semantics changed")
	}
	var foreground map[string]json.RawMessage
	_ = json.Unmarshal(rootFields["Foreground"], &foreground)
	if _, ok := foreground["independent-session"]; !ok {
		t.Fatal("independent active foreground session was removed")
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("retirement facts = %+v", status)
	}
	_, posts := d.counts()
	if posts != 3 { // registration, stale cleanup, rebased cleanup
		t.Fatalf("conditional POST count = %d, want 3", posts)
	}
}

func TestSessionAuthorityCleanupConditionalWritesAreCappedAcrossRetries(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.mu.Lock()
	d.cleanupConflictsRemaining = 6
	d.mu.Unlock()
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("repeated ETag conflicts unexpectedly cleared the route")
	}
	_, posts := d.counts()
	if posts != 1+sessionCleanupAttempts {
		t.Fatalf("cleanup issued %d writes; want registration plus at most %d cleanup attempts", posts, sessionCleanupAttempts)
	}
	d.mu.Lock()
	tags := append([]string(nil), d.writeTags...)
	d.mu.Unlock()
	for i, tag := range tags {
		if !validServeETag(tag) {
			t.Fatalf("POST %d had invalid ETag", i)
		}
		if i > 0 && tag == tags[i-1] {
			t.Fatalf("cleanup replayed stale ETag at POST %d", i)
		}
	}
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("owner retried beyond its three-write cleanup budget")
	}
	_, after := d.counts()
	if after != posts {
		t.Fatalf("cleanup retry count was not persistent: %d -> %d", posts, after)
	}
	d.loseWatch(sessionTestID)
	waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
}

func TestSessionAuthorityRemoteWatcherGCIsUnknownAfterAcknowledgedConfigClear(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.setConfig(addForeignConfig(d.currentConfig()))
	d.mu.Lock()
	d.delayedCleanupOnLocalClose = true
	d.mu.Unlock()
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("Retire did not accept config-level clear: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("local result incorrectly waited for remote watcher GC: %+v", status)
	}
	time.Sleep(80 * time.Millisecond)
	var root map[string]json.RawMessage
	_ = json.Unmarshal(d.currentConfig(), &root)
	var sessions map[string]json.RawMessage
	_ = json.Unmarshal(root["Foreground"], &sessions)
	if _, exists := sessions["independent-session"]; !exists {
		t.Fatal("delayed daemon watcher cleanup removed another foreground key")
	}
}

func TestSessionAuthorityChangedSameKeyQuarantinesWithoutMutationOrIntentionalClose(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	config := d.currentConfig()
	var root map[string]json.RawMessage
	_ = json.Unmarshal(config, &root)
	var sessions map[string]json.RawMessage
	_ = json.Unmarshal(root["Foreground"], &sessions)
	sessions[sessionTestID] = json.RawMessage(`{"TCP":{"443":{"HTTPS":true}},"Web":{"relay.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}}},"Services":{"svc:expanded":{"Tun":true}}}`)
	root["Foreground"], _ = json.Marshal(sessions)
	changed, _ := json.Marshal(root)
	d.setConfig(changed)
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("changed same-session entry was deleted")
	}
	status := authority.Status()
	if !status.Quarantined || status.RouteCleared || status.LocalWatchClosed {
		t.Fatalf("changed-key refusal state = %+v", status)
	}
	select {
	case <-authority.Invalidation():
	default:
		t.Fatal("quarantine did not signal caller invalidation")
	}
	watches, posts := d.counts()
	if watches != 1 || posts != 1 {
		t.Fatalf("changed-key path wrote or intentionally closed: watches=%d posts=%d", watches, posts)
	}

	// Model the pinned daemon's unconditional single-key cleanup before the
	// client observes involuntary watcher loss. This is the documented limit,
	// not a claim that quarantine preserves a same-key replacement.
	d.loseWatch(sessionTestID)
	waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
	var lost map[string]json.RawMessage
	_ = json.Unmarshal(d.currentConfig(), &lost)
	var remaining map[string]json.RawMessage
	_ = json.Unmarshal(lost["Foreground"], &remaining)
	if _, ok := remaining[sessionTestID]; ok {
		t.Fatal("fake pinned watcher exit failed to remove the replaced same-key value")
	}
}

func TestSessionAuthorityWatcherDeleteMapRemovalGapNeverValidatesOrRetries(t *testing.T) {
	d := newSessionDaemon()
	d.watcherDeleteMapRemovalGap = true
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Activate(context.Background()); err != nil {
		t.Fatalf("source-order gap POST disposition = %v", err)
	}
	waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
	if err := authority.Validate(context.Background()); err == nil {
		t.Fatal("route installed in watcher-delete/map-removal gap validated after watch loss")
	}
	status := authority.Status()
	if !status.Invalidated || !status.Quarantined || status.RouteCleared || status.RouteValidated {
		t.Fatalf("watcher-delete/map-removal gap was not quarantined: %+v", status)
	}
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("owner mutated after watcher had already ended")
	}
	_, posts := d.counts()
	if posts != 1 {
		t.Fatalf("watcher-loss gap caused a registration/cleanup retry: %d POSTs", posts)
	}
	if _, exists, err := foregroundEntry(d.currentConfig(), sessionTestID); err != nil || !exists {
		t.Fatalf("fake daemon did not retain the gap-installed exact route: exists=%t err=%v", exists, err)
	}
}

func TestSessionAuthorityAppliedRegistrationWithLostAckIsSelectivelyRemoved(t *testing.T) {
	d := newSessionDaemon()
	d.dropRegistrationAck = true
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Activate(context.Background()); !errors.Is(err, errSessionWrite) {
		t.Fatalf("lost registration acknowledgement = %v", err)
	}
	if status := authority.Status(); status.RegistrationOutcome != "unresolved" || status.RouteCleared {
		t.Fatalf("applied/lost-ack registration state = %+v", status)
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("reconcile exact applied route: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || status.RegistrationOutcome != "unresolved" {
		t.Fatalf("applied/lost-ack cleanup facts = %+v", status)
	}
	_, posts := d.counts()
	if posts != 2 {
		t.Fatalf("ambiguous registration/deletion POST count = %d", posts)
	}
}

func TestSessionAuthorityStalledRegistrationTimeoutAndLateCommitStayQuarantined(t *testing.T) {
	d := newSessionDaemon()
	d.stallRegistration = true
	d.lateApply = make(chan struct{})
	d.lateApplied = make(chan struct{})
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	err := authority.Activate(ctx)
	cancel()
	if !errors.Is(err, errSessionWrite) {
		t.Fatalf("stalled registration disposition = %v", err)
	}
	if err := authority.Retire(context.Background()); err == nil || authority.Status().RouteCleared {
		t.Fatalf("absent GET released stalled registration: err=%v state=%+v", err, authority.Status())
	}
	close(d.lateApply)
	select {
	case <-d.lateApplied:
	case <-time.After(time.Second):
		t.Fatal("late registration did not apply")
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("late route exact cleanup: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || status.RegistrationOutcome != "unresolved" {
		t.Fatalf("late-commit retirement facts = %+v", status)
	}
}

func TestSessionAuthorityUnresolvedRegistrationAbsenceStaysQuarantinedUntilLateRouteIsDeleted(t *testing.T) {
	d := newSessionDaemon()
	d.dropRegistrationWithoutApply = true
	d.lateApply = make(chan struct{})
	d.lateApplied = make(chan struct{})
	authority := newSessionTestAuthority(d)
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := authority.Activate(context.Background()); !errors.Is(err, errSessionWrite) {
		t.Fatalf("ambiguous registration error = %v", err)
	}
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("absent GET settled an unresolved registration")
	}
	status := authority.Status()
	if !status.Quarantined || status.RouteCleared || status.LocalWatchClosed || status.RegistrationOutcome != "unresolved" {
		t.Fatalf("unresolved absent state = %+v", status)
	}
	close(d.lateApply)
	select {
	case <-d.lateApplied:
	case <-time.After(time.Second):
		t.Fatal("late synthetic registration did not apply")
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("late exact route cleanup: %v", err)
	}
	status = authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown || status.RegistrationOutcome != "unresolved" {
		t.Fatalf("late route cleanup state = %+v", status)
	}
	_, posts := d.counts()
	if posts != 2 {
		t.Fatalf("registration/deletion attempt count = %d", posts)
	}
}

func TestSessionAuthorityAmbiguousDeletionDoesNotReplayAndAbsentReadClearsSettledRegistration(t *testing.T) {
	d := newSessionDaemon()
	d.dropDeleteAck = true
	authority := prepareAndActivate(t, d)
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("ambiguous deletion and fresh absence: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("ambiguous cleanup result = %+v", status)
	}
	_, posts := d.counts()
	if posts != 2 {
		t.Fatalf("ambiguous cleanup was replayed: %d POSTs", posts)
	}
}

func TestSessionAuthorityValidationRejectsForeignExposureButRetirementPreservesIt(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.setConfig(addForeignConfig(d.currentConfig()))
	if err := authority.Validate(context.Background()); err == nil {
		t.Fatal("foreign routes/services/Funnel fields passed readiness validation")
	}
	if status := authority.Status(); status.RouteValidated || !status.Invalidated {
		t.Fatalf("foreign exposure did not revoke owner validation: %+v", status)
	}
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("selective retirement preserving foreign exposure: %v", err)
	}
	var remaining map[string]json.RawMessage
	_ = json.Unmarshal(d.currentConfig(), &remaining)
	for _, name := range []string{"Services", "AllowFunnel", "Foreground"} {
		if _, ok := remaining[name]; !ok {
			t.Errorf("retirement dropped foreign field %s", name)
		}
	}
	var sessions map[string]json.RawMessage
	_ = json.Unmarshal(remaining["Foreground"], &sessions)
	if _, ok := sessions["independent-session"]; !ok {
		t.Fatal("retirement deleted an independent foreground key")
	}
}

func TestSessionAuthorityFinalReadCloseGapSameKeyReplacementRemainsAnExplicitLimit(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.mu.Lock()
	d.replaceOnAbsentRead = true
	d.cleanupOnLocalClose = true
	d.mu.Unlock()
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("retire exact route through final-read/close gap: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("final gap retirement facts = %+v", status)
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(d.currentConfig(), &root)
	var sessions map[string]json.RawMessage
	_ = json.Unmarshal(root["Foreground"], &sessions)
	if _, exists := sessions[sessionTestID]; exists {
		t.Fatal("fake daemon failed to model unconditional same-key deletion during watch close")
	}
}

func TestSessionAuthorityUnknownSchemaRefusesCleanupAndWatchClose(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.setConfig([]byte(`{"Foreground":{"` + sessionTestID + `":` + configRouteEntry + `},"Future":true}`))
	if err := authority.Retire(context.Background()); err == nil {
		t.Fatal("unknown Serve field permitted mutation/retirement")
	}
	status := authority.Status()
	if !status.Quarantined || status.RouteCleared || status.LocalWatchClosed {
		t.Fatalf("unknown schema refusal state = %+v", status)
	}
	_, posts := d.counts()
	if posts != 1 {
		t.Fatalf("unknown schema produced cleanup POST: count=%d", posts)
	}
	d.loseWatch(sessionTestID)
	waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
}

func TestSessionAuthorityRetireDeadlineBoundsHeldLifecycleOperation(t *testing.T) {
	daemon := newSessionDaemon()
	authority := prepareAndActivate(t, daemon)
	release, err := authority.opMu.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	started := time.Now()
	err = authority.Retire(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		release()
		t.Fatalf("authority retirement behind held operation = %v after %s", err, time.Since(started))
	}
	if status := authority.Status(); !status.Active || status.RouteCleared || status.LocalWatchClosed {
		release()
		t.Fatalf("timed-out retirement changed live route state: %+v", status)
	}
	_, posts := daemon.counts()
	if posts != 1 {
		release()
		t.Fatalf("timed-out retirement dispatched a later write: POST count=%d", posts)
	}
	release()
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer finishCancel()
	if err := authority.Retire(finishCtx); err != nil {
		t.Fatalf("later authority retirement did not finish: %v", err)
	}
	status := authority.Status()
	if !status.RouteCleared || !status.LocalWatchClosed {
		t.Fatalf("later authority retirement status = %+v", status)
	}
}

func TestSessionAuthorityConcurrentValidateRetireSerializesAndNeverMutatesAfterClear(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- authority.Validate(context.Background()) }()
	go func() { <-start; results <- authority.Retire(context.Background()) }()
	close(start)
	<-results
	<-results
	if !authority.Status().RouteCleared {
		if err := authority.Retire(context.Background()); err != nil {
			t.Fatalf("serialized followup retirement: %v", err)
		}
	}
	beforeWatches, beforePosts := d.counts()
	for i := 0; i < 3; i++ {
		_ = authority.Validate(context.Background())
		_ = authority.Retire(context.Background())
	}
	afterWatches, afterPosts := d.counts()
	if beforeWatches != afterWatches || beforePosts != afterPosts || afterPosts != 2 {
		t.Fatalf("operation after clear: watches %d->%d posts %d->%d", beforeWatches, afterWatches, beforePosts, afterPosts)
	}
}

func TestSessionAuthoritySpontaneousWatchLossUsesFreshAbsenceAndInvalidates(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.loseWatch(sessionTestID)
	status := waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed && s.RouteCleared })
	if !status.Invalidated || !status.RouteCleared || status.RouteValidated || !status.RemoteWatchRetirementUnknown {
		t.Fatalf("spontaneous watcher loss facts = %+v", status)
	}
	if err := authority.Validate(context.Background()); err == nil {
		t.Fatal("watcher-delete-before-client-EOF interleaving still validated")
	}
	_, posts := d.counts()
	if posts != 1 {
		t.Fatalf("watcher map-removal race triggered a registration retry: %d POSTs", posts)
	}
}

func TestSessionAuthorityBackgroundMonitorSignalsRouteDrift(t *testing.T) {
	d := newSessionDaemon()
	authority := prepareAndActivate(t, d)
	d.setConfig(addForeignConfig(d.currentConfig()))
	select {
	case <-authority.Invalidation():
	case <-time.After(3 * time.Second):
		t.Fatal("background owner monitor did not signal foreign route/config drift")
	}
	status := authority.Status()
	if !status.Invalidated || !status.Quarantined || status.RouteValidated || status.LocalWatchClosed {
		t.Fatalf("monitored route drift facts = %+v", status)
	}
	d.loseWatch(sessionTestID)
	waitForSessionStatus(t, authority, func(s AuthorityStatus) bool { return s.LocalWatchClosed })
}

func waitForSessionStatus(t *testing.T, authority *SessionAuthority, predicate func(AuthorityStatus) bool) AuthorityStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := authority.Status()
		if predicate(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	status := authority.Status()
	t.Fatalf("session state did not reach expected state: %+v", status)
	return status
}
