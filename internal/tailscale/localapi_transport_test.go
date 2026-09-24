package tailscale

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLocalAPIProductionConstructorPinsTransport(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("production LocalAPI transport is intentionally unsupported on this OS")
	}
	api, err := newLocalAPI(localAPITestVersion, []byte(sourceVersion))
	if err != nil {
		t.Fatalf("construct pinned LocalAPI transport: %v", err)
	}
	if api.client.OmitAuth != (runtime.GOOS == "darwin") {
		t.Fatalf("OmitAuth = %t on %s", api.client.OmitAuth, runtime.GOOS)
	}
	wrapper, ok := api.client.Transport.(noRedirectLocalAPITransport)
	if !ok {
		t.Fatalf("production transport type = %T", api.client.Transport)
	}
	transport, ok := wrapper.next.(*http.Transport)
	if !ok {
		t.Fatalf("underlying production transport type = %T", wrapper.next)
	}
	if transport.DialContext == nil || transport.Proxy != nil || !transport.DisableCompression || !transport.DisableKeepAlives || transport.MaxResponseHeaderBytes != localAPIDiagnosticLimit {
		t.Fatal("production transport bounds/dial policy are not pinned")
	}
}

func TestLocalAPIConstructorRuntimeGateRejectsUnsupportedTargets(t *testing.T) {
	for _, goos := range []string{"android", "ios", "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			if err := checkLocalAPIRuntime(goos); err == nil {
				t.Fatalf("unsupported runtime %q passed the constructor admission gate", goos)
			}
		})
	}
}

func TestLocalAPIProductionTransportRejectsOversizedWireHeaders(t *testing.T) {
	if err := checkLocalAPIRuntime(runtime.GOOS); err != nil {
		t.Skip("production LocalAPI transport is intentionally unsupported on this OS")
	}
	api, err := newLocalAPI(localAPITestVersion, []byte(sourceVersion))
	if err != nil {
		t.Fatalf("construct production LocalAPI transport: %v", err)
	}
	wrapper, ok := api.client.Transport.(noRedirectLocalAPITransport)
	if !ok {
		t.Fatalf("production transport type = %T", api.client.Transport)
	}
	transport, ok := wrapper.next.(*http.Transport)
	if !ok {
		t.Fatalf("underlying production transport type = %T", wrapper.next)
	}
	writeResult := make(chan struct {
		n   int
		err error
	}, 1)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != localAPIHost+":80" {
			return nil, errors.New("unexpected LocalAPI dial target")
		}
		clientConn, serverConn := net.Pipe()
		_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
		go func() {
			defer serverConn.Close()
			requestReader := bufio.NewReader(serverConn)
			requestComplete := false
			for i := 0; i < 128; i++ {
				line, readErr := requestReader.ReadString('\n')
				if readErr != nil {
					writeResult <- struct {
						n   int
						err error
					}{err: readErr}
					return
				}
				if line == "\r\n" {
					requestComplete = true
					break
				}
			}
			if !requestComplete {
				writeResult <- struct {
					n   int
					err error
				}{err: errors.New("request headers did not terminate")}
				return
			}
			var response bytes.Buffer
			response.WriteString("HTTP/1.1 200 OK\r\n")
			for i := 0; i < 70; i++ {
				response.WriteString("X-Padding: ")
				response.WriteString(strings.Repeat("x", 1024))
				response.WriteString("\r\n")
			}
			response.WriteString("Content-Length: 0\r\n\r\n")
			n, writeErr := serverConn.Write(response.Bytes())
			writeResult <- struct {
				n   int
				err error
			}{n: n, err: writeErr}
		}()
		return clientConn, nil
	}
	defer transport.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+localAPIHost+localAPIStatusPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response, requestErr := api.client.DoLocalRequest(request)
	elapsed := time.Since(started)
	if response != nil {
		_ = response.Body.Close()
	}
	if requestErr == nil || errors.Is(requestErr, context.DeadlineExceeded) {
		t.Fatalf("oversized raw HTTP headers were not promptly refused: response=%v err=%v", response, requestErr)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("oversized HTTP header refusal exceeded request bound: %v", elapsed)
	}
	select {
	case result := <-writeResult:
		if result.n < localAPIDiagnosticLimit {
			t.Fatalf("in-memory peer sent only %d response bytes before refusal (err %v)", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("in-memory response writer did not complete after header refusal")
	}
}

func TestLocalAPITransportRejectsUnconditionalOrReplayableWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		method  string
		headers http.Header
	}{
		{name: "missing etag", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}}},
		{name: "empty etag", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {""}}},
		{name: "wrong content type", method: http.MethodPost, headers: http.Header{"Content-Type": {"text/plain"}, "If-Match": {localAPITestETag}}},
		{name: "idempotency key", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {localAPITestETag}, "Idempotency-Key": {"retry"}}},
		{name: "x idempotency key", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {localAPITestETag}, "X-Idempotency-Key": {"retry"}}},
		{name: "duplicate if-match", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {localAPITestETag, localAPITestETag}}},
		{name: "noncanonical duplicate if-match", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {localAPITestETag}, "if-match": {"foreign"}}},
		{name: "proxy authorization", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {localAPITestETag}, "proxy-authorization": {"", "private"}}},
		{name: "empty idempotency header", method: http.MethodPost, headers: http.Header{"Content-Type": {"application/json"}, "If-Match": {localAPITestETag}, "Idempotency-Key": {""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(tc.method, "http://"+localAPIHost+localAPIServeConfigPath, bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatal(err)
			}
			request.Header = tc.headers
			if validLocalAPIRequest(request) {
				t.Fatal("unsafe write request accepted")
			}
		})
	}

	request, err := http.NewRequest(http.MethodPost, "http://"+localAPIHost+localAPIServeConfigPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", localAPITestETag)
	if !validLocalAPIRequest(request) {
		t.Fatal("complete conditional write refused")
	}
}

func TestLocalAPITransportStopsDefaultHTTPClientRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			transport := noRedirectLocalAPITransport{next: localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != localAPIHost {
					t.Fatalf("unexpected follow-up host %q", request.URL.Host)
				}
				if request.Header.Get("Authorization") != "Basic private-proof" {
					t.Fatal("the first request did not retain its local authentication header")
				}
				return localAPIResponse(request, status, http.Header{
					"Location":          {"http://foreign.invalid/credential-path?proof=private"},
					"Tailscale-Version": {localAPITestVersion},
				}, io.NopCloser(bytes.NewReader(nil))), nil
			})}
			client := &http.Client{Transport: transport}
			request, err := http.NewRequest(http.MethodGet, "http://"+localAPIHost+localAPIStatusPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Basic private-proof")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != status || response.Header.Get("Location") != "" || calls != 1 {
				t.Fatalf("redirect response status=%d location=%q dispatches=%d", response.StatusCode, response.Header.Get("Location"), calls)
			}
		})
	}
}

func TestLocalAPITransportRefusesRemoteOrMalformedTargetsBeforeDial(t *testing.T) {
	calls := 0
	transport := noRedirectLocalAPITransport{next: localAPITestRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not dispatch")
	})}
	for _, target := range []struct {
		method string
		url    string
		host   string
	}{
		{method: http.MethodGet, url: "http://foreign.invalid/localapi/v0/status"},
		{method: http.MethodGet, url: "https://" + localAPIHost + localAPIStatusPath},
		{method: http.MethodGet, url: "http://" + localAPIHost + localAPIStatusPath + "?other=endpoint"},
		{method: http.MethodGet, url: "http://" + localAPIHost + "/localapi/v0/not-allowed"},
	} {
		request, err := http.NewRequest(target.method, target.url, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = target.host
		if _, err := transport.RoundTrip(request); err == nil {
			t.Errorf("malformed target accepted: %s %s", target.method, target.url)
		}
	}
	if calls != 0 {
		t.Fatalf("unsafe targets reached the next transport %d times", calls)
	}
}
