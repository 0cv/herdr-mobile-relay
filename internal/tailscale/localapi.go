package tailscale

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"tailscale.com/client/local"
)

const (
	localAPIHost            = "local-tailscaled.sock"
	localAPIStatusPath      = "/localapi/v0/status"
	localAPIServeConfigPath = "/localapi/v0/serve-config"
	localAPIWatchPath       = "/localapi/v0/watch-ipn-bus"
	localAPIWatchQuery      = "mask=NotifyInitialState"
	localAPIRequestTimeout  = 5 * time.Second
	localAPIWatchMaxEvent   = 1 << 20
	localAPIDiagnosticLimit = 64 << 10
)

var (
	errLocalAPIUnsupportedVersion = errors.New("unsupported Tailscale LocalAPI version")
	errLocalAPIRequest            = errors.New("Tailscale LocalAPI request failed")
	errLocalAPITimeout            = errors.New("Tailscale LocalAPI request timed out")
	errLocalAPIAuthorization      = errors.New("Tailscale LocalAPI authorization denied")
	errLocalAPIResponse           = errors.New("invalid Tailscale LocalAPI response")
	errLocalAPIWriteConflict      = errors.New("Tailscale Serve configuration changed")
	errLocalAPIWatchTimeout       = errors.New("Tailscale LocalAPI watch did not produce an initial event in time")
	errLocalAPIWatchEnded         = errors.New("Tailscale LocalAPI watch ended")
	errLocalAPIWatchCanceled      = errors.New("Tailscale LocalAPI watch canceled")
)

type localAPI struct {
	client          *local.Client
	expectedVersion string
}

// newLocalAPI constructs the pinned production transport. Its endpoint and
// socket selection are intentionally not configurable.
func newLocalAPI(expectedVersion string, versionMetadata []byte) (*localAPI, error) {
	if !localAPIVersionMetadataAllowed(versionMetadata, expectedVersion) {
		return nil, errLocalAPIUnsupportedVersion
	}
	if err := checkLocalAPIRuntime(runtime.GOOS); err != nil {
		return nil, err
	}
	transport, omitAuth, err := newPinnedLocalAPIRoundTripper()
	if err != nil {
		return nil, err
	}
	return &localAPI{
		client:          &local.Client{Transport: transport, OmitAuth: omitAuth},
		expectedVersion: expectedVersion,
	}, nil
}

func localAPIVersionMetadataAllowed(data []byte, expectedVersion string) bool {
	if validateVersion(data, expectedVersion) != nil {
		return false
	}
	fields, err := object(data)
	if err != nil {
		return false
	}
	for _, key := range []string{"extraGitCommit", "osVariant"} {
		var value string
		if scalar(fields, key, &value, false) != nil || value != "" {
			return false
		}
	}
	return true
}

func checkLocalAPIRuntime(goos string) error {
	if goos == "linux" || goos == "darwin" {
		return nil
	}
	return unsupportedPlatformError()
}

func (c *localAPI) status(ctx context.Context) (Status, error) {
	body, err := c.getJSON(ctx, localAPIStatusPath)
	if err != nil {
		return Status{}, err
	}
	status, err := ParseStatus(body)
	if err != nil || status.Version != c.expectedVersion {
		return Status{}, errLocalAPIResponse
	}
	return status, nil
}

// serveConfig returns the exact bounded document and its mandatory source ETag.
// The full Serve schema is deliberately left to the later schema-preserving owner.
func (c *localAPI) serveConfig(ctx context.Context) ([]byte, string, error) {
	resp, cancel, err := c.doOne(ctx, http.MethodGet, localAPIServeConfigPath, nil, "")
	if err != nil {
		return nil, "", err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, "", errLocalAPIAuthorization
	}
	if resp.StatusCode != http.StatusOK || !validResponseVersion(resp.Header, c.expectedVersion) || !jsonContentType(resp.Header) {
		return nil, "", errLocalAPIResponse
	}
	etag, ok := responseETag(resp.Header)
	if !ok {
		return nil, "", errLocalAPIResponse
	}
	body, err := readLocalAPIBody(resp.Body)
	if err != nil {
		return nil, "", errLocalAPIResponse
	}
	if !validServeConfigDocument(body) {
		return nil, "", errLocalAPIResponse
	}
	return body, etag, nil
}

func (c *localAPI) getJSON(ctx context.Context, path string) ([]byte, error) {
	resp, cancel, err := c.doOne(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, errLocalAPIAuthorization
	}
	if resp.StatusCode != http.StatusOK || !validResponseVersion(resp.Header, c.expectedVersion) || !jsonContentType(resp.Header) {
		return nil, errLocalAPIResponse
	}
	body, err := readLocalAPIBody(resp.Body)
	if err != nil || strictJSON(body) != nil {
		return nil, errLocalAPIResponse
	}
	return body, nil
}

func (c *localAPI) doOne(parent context.Context, method, path string, body []byte, etag string) (*http.Response, context.CancelFunc, error) {
	if c == nil || c.client == nil || parent == nil {
		return nil, nil, errLocalAPIRequest
	}
	ctx, cancel := context.WithTimeout(parent, localAPIRequestTimeout)
	request, err := http.NewRequestWithContext(ctx, method, "http://"+localAPIHost+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, errLocalAPIRequest
	}
	request.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", etag)
		// A conditional registration is never replayed, even after a transport
		// failure that proves no request bytes were written.
		request.GetBody = nil
	}
	response, err := c.client.DoLocalRequest(request)
	if err != nil {
		cancel()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(parent.Err(), context.DeadlineExceeded) {
			return nil, nil, errLocalAPITimeout
		}
		return nil, nil, errLocalAPIRequest
	}
	if response == nil || response.Body == nil {
		cancel()
		return nil, nil, errLocalAPIResponse
	}
	return response, cancel, nil
}

func (c *localAPI) setServeConfig(ctx context.Context, etag string, body []byte) localAPIWriteResult {
	refused := func(err error) localAPIWriteResult {
		return localAPIWriteResult{disposition: localAPIWriteNotDispatched, err: err}
	}
	if c == nil || c.client == nil || ctx == nil || !validServeETag(etag) {
		return refused(errLocalAPIRequest)
	}
	if _, err := object(body); err != nil {
		return refused(errLocalAPIResponse)
	}
	requestBody := append([]byte(nil), body...)
	resp, cancel, err := c.doOne(ctx, http.MethodPost, localAPIServeConfigPath, requestBody, etag)
	if err != nil {
		return localAPIWriteResult{disposition: localAPIWriteUnresolved, err: err}
	}
	defer cancel()
	defer resp.Body.Close()
	if !validResponseVersion(resp.Header, c.expectedVersion) {
		return localAPIWriteResult{disposition: localAPIWriteUnresolved, statusCode: resp.StatusCode, err: errLocalAPIResponse}
	}
	responseBody, err := readLocalAPIBody(resp.Body)
	if err != nil {
		return localAPIWriteResult{disposition: localAPIWriteUnresolved, statusCode: resp.StatusCode, err: errLocalAPIResponse}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if len(responseBody) != 0 {
			return localAPIWriteResult{disposition: localAPIWriteUnresolved, statusCode: resp.StatusCode, err: errLocalAPIResponse}
		}
		return localAPIWriteResult{disposition: localAPIWriteSettledSuccess, statusCode: resp.StatusCode}
	case http.StatusPreconditionFailed:
		// In the pinned handler, HTTP 412 is emitted only for an ETag mismatch,
		// before the config write. Require its complete source-shaped error body,
		// but never surface that body.
		if !sourceETagRefusal(resp.Header, responseBody) {
			return localAPIWriteResult{disposition: localAPIWriteUnresolved, statusCode: resp.StatusCode, err: errLocalAPIResponse}
		}
		return localAPIWriteResult{disposition: localAPIWriteSettledNoWrite, statusCode: resp.StatusCode, err: errLocalAPIWriteConflict}
	default:
		return localAPIWriteResult{disposition: localAPIWriteUnresolved, statusCode: resp.StatusCode, err: errLocalAPIResponse}
	}
}

type localAPIWriteDisposition uint8

const (
	localAPIWriteNotDispatched localAPIWriteDisposition = iota
	localAPIWriteSettledSuccess
	localAPIWriteSettledNoWrite
	localAPIWriteUnresolved
)

type localAPIWriteResult struct {
	disposition localAPIWriteDisposition
	statusCode  int
	err         error
}

func validServeETag(etag string) bool {
	if len(etag) != 64 || strings.TrimSpace(etag) != etag {
		return false
	}
	for _, c := range etag {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func sourceETagRefusal(header http.Header, body []byte) bool {
	values := header.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "text/plain" || !bytes.Equal(body, []byte("etag mismatch\n")) {
		return false
	}
	charset, ok := params["charset"]
	return ok && strings.EqualFold(charset, "utf-8")
}

func responseETag(header http.Header) (string, bool) {
	values := header.Values("ETag")
	if len(values) != 1 || !validServeETag(values[0]) {
		return "", false
	}
	return values[0], true
}

func validResponseVersion(header http.Header, expected string) bool {
	values := header.Values("Tailscale-Version")
	return len(values) == 1 && values[0] == expected
}

func validServeConfigDocument(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if bytes.Equal(trimmed, []byte("null")) {
		return strictJSON(body) == nil
	}
	_, err := object(body)
	return err == nil
}

func jsonContentType(header http.Header) bool {
	values := header.Values("Content-Type")
	if len(values) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(values[0])
	if err != nil || mediaType != "application/json" {
		return false
	}
	charset, ok := params["charset"]
	return !ok || strings.EqualFold(charset, "utf-8")
}

func readLocalAPIBody(body io.Reader) ([]byte, error) {
	limited := io.LimitReader(body, MaxOutputBytes+1)
	result, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(result) > MaxOutputBytes {
		return nil, ErrOutputTooLong
	}
	return result, nil
}

type localAPIWatch struct {
	client  *local.Client
	version string
	ctx     context.Context
	cancel  context.CancelFunc

	started         chan error
	doneCh          chan struct{}
	cancelCloseDone chan struct{}

	mu            sync.Mutex
	body          io.ReadCloser
	session       string
	terminalError error
	closedByOwner bool
	closeOnce     sync.Once
	bodyCloseOnce sync.Once
}

// watch opens a long-lived watch and accepts it only after its own initial,
// version-matched event has yielded a nonempty session ID.
func (c *localAPI) watch(ctx context.Context) (*localAPIWatch, error) {
	return c.watchWithFirstEventTimeout(ctx, localAPIRequestTimeout)
}

func (c *localAPI) watchWithFirstEventTimeout(parent context.Context, timeout time.Duration) (*localAPIWatch, error) {
	if c == nil || c.client == nil || parent == nil || timeout <= 0 {
		return nil, errLocalAPIRequest
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithCancel(parent)
	watch := &localAPIWatch{
		client:          c.client,
		version:         c.expectedVersion,
		ctx:             ctx,
		cancel:          cancel,
		started:         make(chan error, 1),
		doneCh:          make(chan struct{}),
		cancelCloseDone: make(chan struct{}),
	}
	context.AfterFunc(ctx, func() {
		watch.closeBody()
		close(watch.cancelCloseDone)
	})
	go watch.run()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-watch.started:
		if err != nil {
			watch.Close()
			return nil, err
		}
		if parent.Err() != nil {
			watch.Close()
			return nil, errLocalAPIWatchCanceled
		}
		if !time.Now().Before(deadline) {
			watch.Close()
			return nil, errLocalAPIWatchTimeout
		}
		return watch, nil
	case <-timer.C:
		watch.Close()
		return nil, errLocalAPIWatchTimeout
	case <-parent.Done():
		watch.Close()
		if errors.Is(parent.Err(), context.DeadlineExceeded) {
			return nil, errLocalAPIWatchTimeout
		}
		return nil, errLocalAPIWatchCanceled
	}
}

func (w *localAPIWatch) run() {
	defer func() {
		w.cancel()
		<-w.cancelCloseDone
		close(w.doneCh)
	}()
	request, err := http.NewRequestWithContext(w.ctx, http.MethodGet, "http://"+localAPIHost+localAPIWatchPath+"?"+localAPIWatchQuery, nil)
	if err != nil {
		w.signalStarted(errLocalAPIRequest)
		return
	}
	request.Header.Set("Accept", "application/json")
	response, err := w.client.DoLocalRequest(request)
	if err != nil {
		w.signalStarted(w.watchRequestError())
		return
	}
	if response == nil || response.Body == nil {
		w.signalStarted(errLocalAPIResponse)
		return
	}
	w.setBody(response.Body)
	defer w.closeBody()
	if response.StatusCode == http.StatusForbidden {
		w.signalStarted(errLocalAPIAuthorization)
		return
	}
	if response.StatusCode != http.StatusOK || !validResponseVersion(response.Header, w.version) || !jsonContentType(response.Header) {
		w.signalStarted(errLocalAPIResponse)
		return
	}
	reader := bufio.NewReaderSize(response.Body, 32<<10)
	line, err := readLocalAPIEvent(reader)
	if err != nil {
		w.signalStarted(w.eventError(err))
		return
	}
	sessionID, err := parseInitialWatchEvent(line, w.version)
	if err != nil {
		w.signalStarted(errLocalAPIResponse)
		return
	}
	w.mu.Lock()
	w.session = sessionID
	w.mu.Unlock()
	w.signalStarted(nil)
	for {
		line, err = readLocalAPIEvent(reader)
		if err != nil {
			w.setTerminalError(w.eventError(err))
			return
		}
		if !validSubsequentWatchEvent(line, w.version) {
			w.setTerminalError(errLocalAPIResponse)
			return
		}
	}
}

func (w *localAPIWatch) signalStarted(err error) {
	select {
	case w.started <- err:
	default:
	}
}

func (w *localAPIWatch) setBody(body io.ReadCloser) {
	w.mu.Lock()
	w.body = body
	closed := w.closedByOwner || w.ctx.Err() != nil
	w.mu.Unlock()
	if closed {
		w.closeBody()
	}
}

func (w *localAPIWatch) closeBody() {
	w.mu.Lock()
	body := w.body
	w.body = nil
	w.mu.Unlock()
	if body != nil {
		w.bodyCloseOnce.Do(func() { _ = body.Close() })
	}
}

func (w *localAPIWatch) eventError(err error) error {
	if errors.Is(err, ErrOutputTooLong) {
		return errLocalAPIResponse
	}
	w.mu.Lock()
	closedByOwner := w.closedByOwner
	w.mu.Unlock()
	if closedByOwner {
		return nil
	}
	if w.ctx.Err() != nil {
		return errLocalAPIWatchCanceled
	}
	return errLocalAPIWatchEnded
}

func (w *localAPIWatch) watchRequestError() error {
	if errors.Is(w.ctx.Err(), context.DeadlineExceeded) {
		return errLocalAPITimeout
	}
	if w.ctx.Err() != nil {
		return errLocalAPIWatchCanceled
	}
	return errLocalAPIRequest
}

func (w *localAPIWatch) setTerminalError(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closedByOwner {
		w.terminalError = err
	}
}

func (w *localAPIWatch) sessionID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.session
}

func (w *localAPIWatch) done() <-chan struct{} { return w.doneCh }

func (w *localAPIWatch) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.terminalError
}

// Close cancels and joins the local reader. It is not a daemon-side watcher
// retirement acknowledgement; callers must reconcile its foreground key first.
func (w *localAPIWatch) Close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closedByOwner = true
		body := w.body
		w.mu.Unlock()
		w.cancel()
		if body != nil {
			w.bodyCloseOnce.Do(func() { _ = body.Close() })
		}
	})
	<-w.doneCh
}

func readLocalAPIEvent(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		part, err := reader.ReadSlice('\n')
		line = append(line, part...)
		if err == nil {
			payload := line[:len(line)-1]
			if len(payload) > localAPIWatchMaxEvent {
				return nil, ErrOutputTooLong
			}
			return payload, nil
		}
		if len(line) > localAPIWatchMaxEvent {
			return nil, ErrOutputTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func parseInitialWatchEvent(data []byte, expectedVersion string) (string, error) {
	fields, err := object(data)
	if err != nil {
		return "", ErrNotJSON
	}
	var version string
	if scalar(fields, "Version", &version, true) != nil || version != expectedVersion {
		return "", ErrNotJSON
	}
	var session string
	if scalar(fields, "SessionID", &session, true) != nil || !validWatchSessionID(session) {
		return "", ErrNotJSON
	}
	return session, nil
}

func validSubsequentWatchEvent(data []byte, expectedVersion string) bool {
	fields, err := object(data)
	if err != nil {
		return false
	}
	if _, exists := fields["SessionID"]; exists {
		return false
	}
	var version string
	return scalar(fields, "Version", &version, true) == nil && version == expectedVersion
}

func validWatchSessionID(session string) bool {
	if session == "" || len(session) > 128 || strings.TrimSpace(session) != session {
		return false
	}
	return strings.IndexFunc(session, unicode.IsControl) == -1
}
