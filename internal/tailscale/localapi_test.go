package tailscale

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

const localAPITestVersion = "1.102.4-tbbcd7d1fc"
const localAPITestETag = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Protocol tests use an in-memory RoundTripper; none reaches a real socket.
type localAPITestRoundTripper func(*http.Request) (*http.Response, error)

func (f localAPITestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newTestLocalAPI(rt http.RoundTripper) *localAPI {
	return &localAPI{
		client:          &local.Client{Transport: noRedirectLocalAPITransport{next: rt}, OmitAuth: true},
		expectedVersion: localAPITestVersion,
	}
}

func localAPIResponse(request *http.Request, status int, header http.Header, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       body,
		Request:    request,
	}
}

func localAPIJSONHeader() http.Header {
	return http.Header{
		"Content-Type":      {"application/json"},
		"Tailscale-Version": {localAPITestVersion},
	}
}

func testBody(body string) io.ReadCloser { return io.NopCloser(strings.NewReader(body)) }

func TestLocalAPIStatusAndServeConfigBoundedRequests(t *testing.T) {
	calls := 0
	api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Scheme != "http" || request.URL.Host != localAPIHost || request.URL.RawQuery != "" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("unexpected request target or Accept header: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" {
			t.Fatal("inert test transport unexpectedly received authentication")
		}
		if request.Header.Get("Tailscale-Cap") == "" {
			t.Fatal("upstream DoLocalRequest did not add Tailscale-Cap")
		}
		switch request.URL.Path {
		case localAPIStatusPath:
			if request.Method != http.MethodGet {
				t.Fatalf("status method = %s", request.Method)
			}
			return localAPIResponse(request, http.StatusOK, localAPIJSONHeader(), testBody(sourceStatus)), nil
		case localAPIServeConfigPath:
			if request.Method != http.MethodGet {
				t.Fatalf("config method = %s", request.Method)
			}
			header := localAPIJSONHeader()
			header.Set("ETag", localAPITestETag)
			return localAPIResponse(request, http.StatusOK, header, testBody("{}")), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, errors.New("unexpected path")
		}
	}))

	status, err := api.status(context.Background())
	if err != nil || !status.LoggedIn || status.Version != localAPITestVersion {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
	config, etag, err := api.serveConfig(context.Background())
	if err != nil || string(config) != "{}" || etag != localAPITestETag {
		t.Fatalf("config = %q, etag = %q, err = %v", config, etag, err)
	}
	if calls != 2 {
		t.Fatalf("LocalAPI request count = %d, want 2", calls)
	}
}

func TestLocalAPIServeConfigAcceptsPinnedNullAbsence(t *testing.T) {
	api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
		header := localAPIJSONHeader()
		header.Set("ETag", localAPITestETag)
		return localAPIResponse(request, http.StatusOK, header, testBody("null")), nil
	}))
	body, etag, err := api.serveConfig(context.Background())
	if err != nil || string(body) != "null" || etag != localAPITestETag {
		t.Fatalf("source null config = %q, etag = %q, err = %v", body, etag, err)
	}
}

func TestLocalAPIStatusAndConfigRejectIncompleteDocuments(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body []byte
	}{
		{name: "status trailing document", path: localAPIStatusPath, body: []byte(sourceStatus + ` {}`)},
		{name: "status invalid utf8", path: localAPIStatusPath, body: append([]byte(`{"x":"`), 0xff, '"', '}')},
		{name: "config valid prefix", path: localAPIServeConfigPath, body: []byte(`{} {}`)},
		{name: "config null prefix", path: localAPIServeConfigPath, body: []byte(`null {}`)},
		{name: "config duplicate key", path: localAPIServeConfigPath, body: []byte(`{"TCP":{},"TCP":{}}`)},
		{name: "config oversized", path: localAPIServeConfigPath, body: []byte(strings.Repeat(" ", MaxOutputBytes) + "{}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
				header := localAPIJSONHeader()
				if tc.path == localAPIServeConfigPath {
					header.Set("ETag", localAPITestETag)
				}
				return localAPIResponse(request, http.StatusOK, header, io.NopCloser(bytes.NewReader(tc.body))), nil
			}))
			if tc.path == localAPIStatusPath {
				if _, err := api.status(context.Background()); err == nil {
					t.Fatal("invalid status document accepted")
				}
				return
			}
			if _, _, err := api.serveConfig(context.Background()); err == nil {
				t.Fatal("invalid config document accepted")
			}
		})
	}
}

func TestLocalAPIStatusAndConfigRejectVersionAndHeaderDrift(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(http.Header)
		statusFailure bool
	}{
		{name: "missing version", mutate: func(h http.Header) { h.Del("Tailscale-Version") }, statusFailure: true},
		{name: "wrong version", mutate: func(h http.Header) { h.Set("Tailscale-Version", "foreign-version") }, statusFailure: true},
		{name: "duplicate version", mutate: func(h http.Header) { h.Add("Tailscale-Version", localAPITestVersion) }, statusFailure: true},
		{name: "wrong content type", mutate: func(h http.Header) { h.Set("Content-Type", "text/plain") }, statusFailure: true},
		{name: "missing config ETag", mutate: func(h http.Header) { h.Del("ETag") }},
		{name: "empty config ETag", mutate: func(h http.Header) { h.Set("ETag", "") }},
		{name: "duplicate config ETag", mutate: func(h http.Header) { h.Add("ETag", localAPITestETag) }},
		{name: "malformed config ETag", mutate: func(h http.Header) { h.Set("ETag", strings.Repeat("A", 64)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newAPI := func() *localAPI {
				return newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
					header := localAPIJSONHeader()
					body := sourceStatus
					if request.URL.Path == localAPIServeConfigPath {
						header.Set("ETag", localAPITestETag)
						body = "{}"
					}
					tc.mutate(header)
					return localAPIResponse(request, http.StatusOK, header, testBody(body)), nil
				}))
			}
			if tc.statusFailure {
				if _, err := newAPI().status(context.Background()); err == nil {
					t.Fatal("status version/header drift accepted")
				}
			}
			if _, _, err := newAPI().serveConfig(context.Background()); err == nil {
				t.Fatal("config version/header drift accepted")
			}
		})
	}
}

func TestLocalAPIOneShotAuthorizationDenialIsSanitized(t *testing.T) {
	for _, path := range []string{localAPIStatusPath, localAPIServeConfigPath} {
		t.Run(path, func(t *testing.T) {
			api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
				return localAPIResponse(request, http.StatusForbidden, http.Header{}, testBody("private authorization detail")), nil
			}))
			var err error
			if path == localAPIStatusPath {
				_, err = api.status(context.Background())
			} else {
				_, _, err = api.serveConfig(context.Background())
			}
			if err != errLocalAPIAuthorization {
				t.Fatalf("authorization error = %v", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("authorization response leaked: %v", err)
			}
		})
	}
}

func TestLocalAPIConditionalWriteClassifiesCompleteAndAmbiguousResults(t *testing.T) {
	truncated := &failingReadCloser{reader: strings.NewReader("partial response"), err: errors.New("private read failure")}
	cases := []struct {
		name         string
		status       int
		version      string
		body         io.ReadCloser
		transportErr error
		want         localAPIWriteDisposition
	}{
		{name: "complete success", status: http.StatusOK, version: localAPITestVersion, body: testBody(""), want: localAPIWriteSettledSuccess},
		{name: "etag refusal", status: http.StatusPreconditionFailed, version: localAPITestVersion, body: testBody("etag mismatch\n"), want: localAPIWriteSettledNoWrite},
		{name: "wrong refusal body", status: http.StatusPreconditionFailed, version: localAPITestVersion, body: testBody("foreign daemon detail"), want: localAPIWriteUnresolved},
		{name: "empty refusal", status: http.StatusPreconditionFailed, version: localAPITestVersion, body: testBody(""), want: localAPIWriteUnresolved},
		{name: "unexpected status", status: http.StatusInternalServerError, version: localAPITestVersion, body: testBody("private error body"), want: localAPIWriteUnresolved},
		{name: "wrong version", status: http.StatusOK, version: "wrong-version", body: testBody(""), want: localAPIWriteUnresolved},
		{name: "unexpected success body", status: http.StatusOK, version: localAPITestVersion, body: testBody("not the source response"), want: localAPIWriteUnresolved},
		{name: "truncated response", status: http.StatusPreconditionFailed, version: localAPITestVersion, body: truncated, want: localAPIWriteUnresolved},
		{name: "transport reset", transportErr: errors.New("private reset detail"), want: localAPIWriteUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Method != http.MethodPost || request.URL.Path != localAPIServeConfigPath || request.URL.RawQuery != "" {
					t.Fatalf("unexpected write request: %s %s", request.Method, request.URL)
				}
				if request.Header.Get("If-Match") != localAPITestETag || request.Header.Get("Content-Type") != "application/json" {
					t.Fatal("conditional JSON headers missing")
				}
				if request.Header.Get("Idempotency-Key") != "" || request.Header.Get("X-Idempotency-Key") != "" || request.GetBody != nil {
					t.Fatal("write unexpectedly opted into HTTP transport retries")
				}
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				header := http.Header{"Tailscale-Version": {tc.version}}
				if tc.status == http.StatusPreconditionFailed {
					header.Set("Content-Type", "text/plain; charset=utf-8")
				}
				return localAPIResponse(request, tc.status, header, tc.body), nil
			}))
			result := api.setServeConfig(context.Background(), localAPITestETag, []byte("{}"))
			if result.disposition != tc.want {
				t.Fatalf("write disposition = %d, want %d (err %v)", result.disposition, tc.want, result.err)
			}
			if calls != 1 {
				t.Fatalf("write dispatch count = %d, want 1", calls)
			}
			if strings.Contains(fmt.Sprint(result.err), "private") || strings.Contains(fmt.Sprint(result.err), "foreign daemon detail") {
				t.Fatalf("diagnostic disclosed transport/body data: %v", result.err)
			}
		})
	}
}

func TestLocalAPIConditionalWriteValidationDoesNotDispatch(t *testing.T) {
	calls := 0
	api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		return localAPIResponse(request, http.StatusOK, http.Header{"Tailscale-Version": {localAPITestVersion}}, testBody("")), nil
	}))
	for _, tc := range []struct {
		etag string
		body []byte
	}{
		{etag: "", body: []byte("{}")},
		{etag: "   ", body: []byte("{}")},
		{etag: strings.Repeat("A", 64), body: []byte("{}")},
		{etag: localAPITestETag, body: []byte(`{} {}`)},
		{etag: localAPITestETag, body: []byte("null")},
		{etag: localAPITestETag, body: []byte("[]")},
		{etag: localAPITestETag, body: []byte(strings.Repeat(" ", MaxOutputBytes) + "{}")},
	} {
		result := api.setServeConfig(context.Background(), tc.etag, tc.body)
		if result.disposition != localAPIWriteNotDispatched {
			t.Fatalf("invalid local request was not refused pre-dispatch: %+v", result)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid local requests dispatched %d times", calls)
	}
}

func TestLocalAPIConditionalWriteTimeoutIsUnresolvedAndSingleAttempt(t *testing.T) {
	calls := 0
	api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := api.setServeConfig(ctx, localAPITestETag, []byte("{}"))
	if result.disposition != localAPIWriteUnresolved || calls != 1 {
		t.Fatalf("timeout result = %+v, calls = %d", result, calls)
	}
	if result.err != errLocalAPITimeout {
		t.Fatalf("timeout diagnostic = %v", result.err)
	}
}

func TestLocalAPIRedirectsNeverDispatchFollowup(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			api := newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != localAPIHost {
					t.Fatalf("redirect dispatched to %q", request.URL.Host)
				}
				if request.Header.Get("Authorization") != "" {
					t.Fatal("redirect request forwarded authorization")
				}
				header := http.Header{"Location": {"http://foreign.invalid/secret-session?token=private"}, "Tailscale-Version": {localAPITestVersion}}
				return localAPIResponse(request, status, header, testBody("private redirect body")), nil
			}))
			result := api.setServeConfig(context.Background(), localAPITestETag, []byte("{}"))
			if result.disposition != localAPIWriteUnresolved || calls != 1 {
				t.Fatalf("redirect result = %+v, dispatch count = %d", result, calls)
			}
			if strings.Contains(fmt.Sprint(result.err), "private") || strings.Contains(fmt.Sprint(result.err), "foreign.invalid") {
				t.Fatalf("redirect details leaked: %v", result.err)
			}
		})
	}
}

func TestLocalAPITransportRejectsEndpointEscape(t *testing.T) {
	transport := noRedirectLocalAPITransport{next: localAPITestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid endpoint reached the next transport")
		return nil, errors.New("unreachable")
	})}
	for _, rawURL := range []string{
		"http://foreign.invalid/localapi/v0/status",
		"https://local-tailscale.sock/localapi/v0/status",
		"http://local-tailscaled.sock/localapi/v0/status?redirect=1",
		"http://local-tailscaled.sock/localapi/v0/unknown",
	} {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(request); err == nil {
			t.Errorf("escaped endpoint accepted: %s", rawURL)
		}
	}
}

type failingReadCloser struct {
	reader io.Reader
	err    error
}

func (r *failingReadCloser) Read(buffer []byte) (int, error) {
	if r.reader == nil {
		return 0, r.err
	}
	n, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		return n, r.err
	}
	return n, err
}
func (r *failingReadCloser) Close() error { return nil }

type trackedReadCloser struct {
	io.ReadCloser
	closeOnce sync.Once
	closed    chan struct{}
}

func newTrackedReadCloser(reader io.ReadCloser) *trackedReadCloser {
	return &trackedReadCloser{ReadCloser: reader, closed: make(chan struct{})}
}
func (r *trackedReadCloser) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		_ = r.ReadCloser.Close()
	})
	return nil
}

func watchNotification(version, session string) string {
	return fmt.Sprintf(`{"Version":%q,"SessionID":%q}`, version, session)
}

func newTestWatchAPI(body io.ReadCloser, header http.Header, status int) *localAPI {
	return newTestLocalAPI(localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != localAPIHost || request.URL.Path != localAPIWatchPath || request.URL.RawQuery != localAPIWatchQuery {
			return nil, errors.New("unexpected watch request")
		}
		if request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" {
			return nil, errors.New("test request unexpectedly authenticated")
		}
		return localAPIResponse(request, status, header, body), nil
	}))
}

func TestLocalAPIWatchMaskMatchesPinnedNumericContract(t *testing.T) {
	encoded, err := ipn.NotifyInitialState.MarshalText()
	if err != nil {
		t.Fatalf("marshal pinned watch mask: %v", err)
	}
	if string(encoded) != "2" {
		t.Fatalf("pinned NotifyInitialState encoding = %q, want numeric mask 2", encoded)
	}
	var decoded ipn.NotifyWatchOpt
	if err := decoded.UnmarshalText([]byte("2")); err != nil || decoded != ipn.NotifyInitialState {
		t.Fatalf("pinned numeric watch mask decode = %v, err = %v", decoded, err)
	}
	if localAPIWatchQuery != "mask=2" {
		t.Fatalf("LocalAPI watch query = %q, want literal pinned protocol mask=2", localAPIWatchQuery)
	}
}

func TestLocalAPIWatchYieldsItsInitialSessionAndJoinsOnClose(t *testing.T) {
	reader, writer := io.Pipe()
	body := newTrackedReadCloser(reader)
	api := newTestWatchAPI(body, localAPIJSONHeader(), http.StatusOK)
	go func() {
		_, _ = io.WriteString(writer, watchNotification(localAPITestVersion, "session-owned-by-this-watch")+"\n")
	}()
	watch, err := api.watch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := watch.sessionID(); got != "session-owned-by-this-watch" {
		t.Fatalf("initial watch session = %q", got)
	}
	watch.Close()
	select {
	case <-watch.done():
	case <-time.After(time.Second):
		t.Fatal("watch reader was not joined")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("watch body was not closed")
	}
	if watch.err() != nil {
		t.Fatalf("intentional local close reported an unexpected stream error: %v", watch.err())
	}
	_ = writer.Close()
}

func TestLocalAPIWatchRejectsInvalidInitialEvents(t *testing.T) {
	deep := `{"Version":"` + localAPITestVersion + `","SessionID":"sid","nested":` + strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34) + "}\n"
	invalidUTF8 := append([]byte(`{"Version":"`+localAPITestVersion+`","SessionID":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`+"\n")...)
	cases := []struct {
		name string
		body []byte
	}{
		{name: "missing session", body: []byte(`{"Version":"` + localAPITestVersion + `"}` + "\n")},
		{name: "empty session", body: []byte(watchNotification(localAPITestVersion, "") + "\n")},
		{name: "whitespace session", body: []byte(watchNotification(localAPITestVersion, " session ") + "\n")},
		{name: "non-string session", body: []byte(`{"Version":"` + localAPITestVersion + `","SessionID":12}` + "\n")},
		{name: "duplicate session", body: []byte(`{"Version":"` + localAPITestVersion + `","SessionID":"a","SessionID":"b"}` + "\n")},
		{name: "case conflict", body: []byte(`{"Version":"` + localAPITestVersion + `","SessionID":"a","sessionid":"b"}` + "\n")},
		{name: "wrong event version", body: []byte(watchNotification("wrong-version", "sid") + "\n")},
		{name: "malformed", body: []byte(`{"Version":` + localAPITestVersion + `}` + "\n")},
		{name: "deep", body: []byte(deep)},
		{name: "invalid utf8", body: invalidUTF8},
		{name: "empty event", body: []byte("\n")},
		{name: "oversized", body: []byte(`{"Version":"` + localAPITestVersion + `","SessionID":"sid","padding":"` + strings.Repeat("x", localAPIWatchMaxEvent) + `"}` + "\n")},
		{name: "unterminated", body: []byte(watchNotification(localAPITestVersion, "sid"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestWatchAPI(io.NopCloser(bytes.NewReader(tc.body)), localAPIJSONHeader(), http.StatusOK)
			watch, err := api.watch(context.Background())
			if err == nil || watch != nil {
				t.Fatalf("invalid initial event accepted: watch=%v err=%v", watch, err)
			}
			if strings.Contains(err.Error(), "sid") || strings.Contains(err.Error(), "session-owned") {
				t.Fatalf("session identifier leaked in diagnostic: %v", err)
			}
		})
	}
}

func TestLocalAPIWatchRejectsBadHeadersAndDeniedAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
	}{
		{name: "wrong version header", status: http.StatusOK, header: http.Header{"Content-Type": {"application/json"}, "Tailscale-Version": {"foreign-version"}}},
		{name: "missing content type", status: http.StatusOK, header: http.Header{"Tailscale-Version": {localAPITestVersion}}},
		{name: "denied authorization", status: http.StatusForbidden, header: http.Header{"Tailscale-Version": {localAPITestVersion}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := testBody("private token and session")
			api := newTestWatchAPI(body, tc.header, tc.status)
			watch, err := api.watch(context.Background())
			if err == nil || watch != nil {
				t.Fatalf("invalid watch response accepted: watch=%v err=%v", watch, err)
			}
			if strings.Contains(err.Error(), "private token") || strings.Contains(err.Error(), "session") {
				t.Fatalf("sensitive response disclosed: %v", err)
			}
		})
	}
}

func TestLocalAPIWatchRejectsMalformedSubsequentEvents(t *testing.T) {
	badEvents := []struct {
		name  string
		event string
		eof   bool
	}{
		{name: "duplicate field", event: `{"Version":"` + localAPITestVersion + `","Version":"` + localAPITestVersion + `"}`},
		{name: "unexpected session", event: watchNotification(localAPITestVersion, "replacement-session")},
		{name: "empty line", event: ""},
		{name: "invalid json", event: "not-json"},
		{name: "eof after initial event", eof: true},
	}
	for _, tc := range badEvents {
		t.Run(tc.name, func(t *testing.T) {
			suffix := tc.event + "\n"
			if tc.eof {
				suffix = ""
			}
			body := io.NopCloser(strings.NewReader(watchNotification(localAPITestVersion, "owned-session") + "\n" + suffix))
			api := newTestWatchAPI(body, localAPIJSONHeader(), http.StatusOK)
			watch, err := api.watch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-watch.done():
			case <-time.After(time.Second):
				watch.Close()
				t.Fatal("malformed subsequent event was not rejected")
			}
			if watch.err() == nil {
				t.Fatal("malformed subsequent event or EOF had no terminal diagnostic")
			}
			if tc.eof && watch.err() != errLocalAPIWatchEnded {
				t.Fatalf("EOF diagnostic = %v", watch.err())
			}
			if strings.Contains(watch.err().Error(), "owned-session") || strings.Contains(watch.err().Error(), "replacement-session") {
				t.Fatalf("session ID leaked in terminal diagnostic: %v", watch.err())
			}
			watch.Close()
		})
	}
}

func TestLocalAPIWatchFirstEventDeadlineClosesAndJoins(t *testing.T) {
	reader, writer := io.Pipe()
	body := newTrackedReadCloser(reader)
	api := newTestWatchAPI(body, localAPIJSONHeader(), http.StatusOK)
	start := time.Now()
	watch, err := api.watchWithFirstEventTimeout(context.Background(), 20*time.Millisecond)
	if err != errLocalAPIWatchTimeout || watch != nil {
		t.Fatalf("delayed first event result: watch=%v err=%v", watch, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("first event bound took %v", elapsed)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("timed-out watch body was not closed")
	}
	_ = writer.Close()
}

func TestLocalAPIWatchParentDeadlineIsTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	reader, writer := io.Pipe()
	body := newTrackedReadCloser(reader)
	api := newTestWatchAPI(body, localAPIJSONHeader(), http.StatusOK)
	watch, err := api.watchWithFirstEventTimeout(ctx, time.Second)
	if watch != nil || err != errLocalAPIWatchTimeout {
		t.Fatalf("parent deadline result: watch=%v err=%v", watch, err)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("parent deadline did not close the response")
	}
	_ = writer.Close()
}

func TestLocalAPIWatchCallerCancellationClosesAndJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	body := newTrackedReadCloser(reader)
	api := newTestWatchAPI(body, localAPIJSONHeader(), http.StatusOK)
	go func() {
		_, _ = io.WriteString(writer, watchNotification(localAPITestVersion, "cancel-owned-watch")+"\n")
	}()
	watch, err := api.watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-watch.done():
	case <-time.After(time.Second):
		watch.Close()
		t.Fatal("caller cancellation did not close and join the watch")
	}
	if watch.err() != errLocalAPIWatchCanceled {
		t.Fatalf("canceled watch diagnostic = %v", watch.err())
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("caller cancellation did not close response body")
	}
	watch.Close()
	_ = writer.Close()
}

func TestLocalAPIWatchResponseHeaderVersionAndWholeEventBounds(t *testing.T) {
	api := newTestWatchAPI(testBody(watchNotification(localAPITestVersion, "owned")+"\n"), localAPIJSONHeader(), http.StatusOK)
	watch, err := api.watch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	watch.Close()

	reader := bufio.NewReader(strings.NewReader(watchNotification(localAPITestVersion, "sid")))
	if _, err := readLocalAPIEvent(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("unterminated event error = %v", err)
	}
}

func TestLocalAPIExpectedVersionMustMatchPinnedSource(t *testing.T) {
	if err := validateVersion([]byte(sourceVersion), localAPITestVersion); err != nil {
		t.Fatalf("pinned source metadata refused: %v", err)
	}
	for _, tc := range []struct {
		version  string
		metadata string
	}{
		{version: "1.102.5-tbbcd7d1fc", metadata: strings.ReplaceAll(sourceVersion, "1.102.4", "1.102.5")},
		{version: "1.102.4-dev-tbbcd7d1fc", metadata: strings.ReplaceAll(sourceVersion, sourceLong, "1.102.4-dev-tbbcd7d1fc")},
		{version: "1.102.4-t0000000", metadata: strings.ReplaceAll(sourceVersion, sourceLong, "1.102.4-t0000000")},
		{version: "1.102.4-tbbcd7d1fc-dirty", metadata: strings.ReplaceAll(sourceVersion, sourceLong, "1.102.4-tbbcd7d1fc-dirty")},
	} {
		if validateVersion([]byte(tc.metadata), tc.version) == nil {
			t.Errorf("unsupported expected version accepted: %q", tc.version)
		}
	}
	if !localAPIVersionMetadataAllowed([]byte(sourceVersion), localAPITestVersion) {
		t.Fatal("pinned source metadata refused by LocalAPI admission")
	}
}

func TestLocalAPIConstructorRejectsUnqualifiedSourceVariants(t *testing.T) {
	extra := strings.Repeat("a", 40)
	long := sourceLong + "-gaaaaaaa"
	supplemental := strings.ReplaceAll(sourceVersion, sourceLong, long)
	supplemental = strings.Replace(supplemental, `"cap":141`, `"cap":141,"extraGitCommit":"`+extra+`"`, 1)
	osVariant := strings.Replace(sourceVersion, `"cap":141`, `"cap":141,"osVariant":"macappstore"`, 1)
	for _, tc := range []struct {
		name     string
		version  string
		metadata string
	}{
		{name: "unpinned supplemental source", version: long, metadata: supplemental},
		{name: "unqualified OS variant", version: localAPITestVersion, metadata: osVariant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, err := newLocalAPI(tc.version, []byte(tc.metadata))
			if api != nil || err != errLocalAPIUnsupportedVersion {
				t.Fatalf("unqualified source variant constructed LocalAPI: api=%v err=%v", api, err)
			}
		})
	}
}
