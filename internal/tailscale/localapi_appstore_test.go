package tailscale

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"
)

const appStoreTestPort = 43123
const appStoreTestToken = "synthetic-appstore-proof"
const appStoreTestGitCommit = "bbcd7d1fc2054b9189ebc1531acf74bd880ca0c9"

func appStoreTestProfile() appStoreLocalAPIProfile {
	return appStoreLocalAPIProfile{
		osVariant:  "macappstore",
		gitCommit:  appStoreTestGitCommit,
		long:       localAPITestVersion,
		daemonLong: localAPITestVersion,
		cap:        141,
	}
}

func appStoreTestMetadata() []byte {
	metadata := strings.Replace(sourceVersion, SourceCommit, appStoreTestGitCommit, 1)
	return []byte(strings.Replace(metadata, `"cap":141`, `"osVariant":"macappstore","cap":141`, 1))
}

func syntheticAppStoreDiscovery(port int, token string) func() (int, string, error) {
	return func() (int, string, error) { return port, token, nil }
}

func TestAppStoreLocalAPIDisabledRefusesBeforeDiscovery(t *testing.T) {
	var discoveries, dials int
	api, err := newAppStoreLocalAPIWith("darwin", appStoreLocalAPIProfileEnabled, appStoreLocalAPIProfiles,
		localAPITestVersion, appStoreTestMetadata(),
		func() (int, string, error) { discoveries++; return appStoreTestPort, appStoreTestToken, nil },
		func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("unexpected dial")
		})
	if api != nil || err != errLocalAPIUnsupportedVersion {
		t.Fatalf("disabled App Store constructor = (%v, %v), want fixed refusal", api, err)
	}
	if discoveries != 0 || dials != 0 {
		t.Fatalf("disabled constructor called discovery %d times and dial %d times", discoveries, dials)
	}
}

func TestAppStoreLocalAPIDiscoveryBoundsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		port  int
		token string
		err   error
	}{
		{name: "discovery error", port: appStoreTestPort, token: appStoreTestToken, err: errors.New("synthetic error")},
		{name: "zero port", port: 0, token: appStoreTestToken},
		{name: "negative port", port: -1, token: appStoreTestToken},
		{name: "oversized port", port: 65536, token: appStoreTestToken},
		{name: "empty token", port: appStoreTestPort},
		{name: "oversized token", port: appStoreTestPort, token: strings.Repeat("x", 257)},
		{name: "colon token", port: appStoreTestPort, token: "x:y"},
		{name: "space token", port: appStoreTestPort, token: "x y"},
		{name: "control token", port: appStoreTestPort, token: "x\ny"},
		{name: "non-ascii token", port: appStoreTestPort, token: "tøken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, token, err := boundedAppStoreDiscovery(func() (int, string, error) { return tc.port, tc.token, tc.err })
			if err != errLocalAPIDiscovery || port != 0 || token != "" {
				t.Fatalf("invalid discovery returned port=%d token=%q err=%v", port, token, err)
			}
		})
	}
	port, token, err := boundedAppStoreDiscovery(syntheticAppStoreDiscovery(65535, strings.Repeat("A", 256)))
	if err != nil || port != 65535 || token != strings.Repeat("A", 256) {
		t.Fatalf("maximum valid discovery = (%d, %q, %v)", port, token, err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	startedAt := time.Now()
	_, _, err = boundedAppStoreDiscovery(func() (int, string, error) {
		close(started)
		<-release
		return appStoreTestPort, appStoreTestToken, nil
	})
	close(release)
	if err != errLocalAPIDiscovery || time.Since(startedAt) < appStoreLocalAPIDiscoveryTimeout {
		t.Fatalf("non-cancellable lookup deadline err=%v elapsed=%v", err, time.Since(startedAt))
	}
	select {
	case <-started:
	default:
		t.Fatal("discovery seam was not invoked")
	}

	const shortTimeout = 15 * time.Millisecond
	lateDiscoveryFinished := make(chan struct{})
	port, token, err = boundedAppStoreDiscoveryWithTimeout(func() (int, string, error) {
		time.Sleep(shortTimeout + 20*time.Millisecond)
		close(lateDiscoveryFinished)
		return appStoreTestPort, appStoreTestToken, nil
	}, shortTimeout)
	if err != errLocalAPIDiscovery || port != 0 || token != "" {
		t.Fatalf("late successful discovery returned port=%d token=%q err=%v", port, token, err)
	}
	select {
	case <-lateDiscoveryFinished:
	case <-time.After(time.Second):
		t.Fatal("late synthetic discovery did not finish")
	}
}

func TestAppStoreLocalAPIDialsOnlyDiscoveredLoopbackPort(t *testing.T) {
	var calls int
	var gotNetwork, gotAddress string
	dialer := appStoreLocalAPIDialer(appStoreTestPort, func(_ context.Context, network, address string) (net.Conn, error) {
		calls++
		gotNetwork, gotAddress = network, address
		client, peer := net.Pipe()
		_ = peer.Close()
		return client, nil
	})
	conn, err := dialer(context.Background(), "tcp", localAPIHost+":80")
	if err != nil {
		t.Fatalf("valid fixed LocalAPI target refused: %v", err)
	}
	_ = conn.Close()
	if calls != 1 || gotNetwork != "tcp" || gotAddress != "127.0.0.1:43123" {
		t.Fatalf("dial calls=%d target=%q %q", calls, gotNetwork, gotAddress)
	}

	for _, target := range []struct{ network, address string }{
		{"udp", localAPIHost + ":80"},
		{"unix", localAPIHost + ":80"},
		{"tcp", localAPIHost + ":81"},
		{"tcp", "localhost:80"},
		{"tcp", "127.0.0.1:80"},
		{"tcp", "[::1]:80"},
		{"tcp", "192.0.2.1:80"},
		{"tcp", "foreign.invalid:80"},
	} {
		if conn, err := dialer(context.Background(), target.network, target.address); err == nil {
			_ = conn.Close()
			t.Errorf("unexpected target accepted: %s %s", target.network, target.address)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid target reached loopback dial function; calls=%d", calls)
	}

	dispatches := 0
	transport := noRedirectLocalAPITransport{next: localAPITestRoundTripper(func(*http.Request) (*http.Response, error) {
		dispatches++
		return nil, errors.New("unexpected dispatch")
	})}
	for _, rawURL := range []string{
		"http://localhost/localapi/v0/status",
		"http://127.0.0.1/localapi/v0/status",
		"http://[::1]/localapi/v0/status",
		"http://192.0.2.1/localapi/v0/status",
		"http://foreign.invalid/localapi/v0/status",
		"http://" + localAPIHost + ":81/localapi/v0/status",
	} {
		request, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(request); err == nil {
			t.Errorf("noncanonical endpoint accepted: %s", rawURL)
		}
	}
	if dispatches != 0 {
		t.Fatalf("noncanonical endpoint reached transport %d times", dispatches)
	}
}

func TestAppStoreLocalAPIRejectsInvalidIdentityAndVersion(t *testing.T) {
	profile := appStoreTestProfile()
	metadata := string(appStoreTestMetadata())
	cases := []struct {
		name     string
		metadata string
		version  string
	}{
		{name: "missing os variant", metadata: strings.Replace(metadata, `"osVariant":"macappstore",`, "", 1), version: localAPITestVersion},
		{name: "partial os variant", metadata: strings.Replace(metadata, `"osVariant":"macappstore"`, `"osVariant":""`, 1), version: localAPITestVersion},
		{name: "different os variant", metadata: strings.Replace(metadata, `"osVariant":"macappstore"`, `"osVariant":"macos"`, 1), version: localAPITestVersion},
		{name: "missing git commit", metadata: strings.Replace(metadata, `"gitCommit":"`+appStoreTestGitCommit+`",`, "", 1), version: localAPITestVersion},
		{name: "different git commit", metadata: strings.Replace(metadata, appStoreTestGitCommit, strings.Repeat("0", 40), 1), version: localAPITestVersion},
		{name: "extra git commit mismatch", metadata: strings.Replace(metadata, `"cap":141`, `"extraGitCommit":"`+strings.Repeat("a", 40)+`","cap":141`, 1), version: localAPITestVersion},
		{name: "long mismatch", metadata: strings.Replace(metadata, `"long":"`+localAPITestVersion+`"`, `"long":"1.102.4-other"`, 1), version: localAPITestVersion},
		{name: "daemon long mismatch", metadata: strings.Replace(metadata, `"daemonLong":"`+localAPITestVersion+`"`, `"daemonLong":"1.102.4-other"`, 1), version: localAPITestVersion},
		{name: "cap mismatch", metadata: strings.Replace(metadata, `"cap":141`, `"cap":142`, 1), version: localAPITestVersion},
		{name: "expected version mismatch", metadata: metadata, version: "1.102.4-other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			discoveries, dials := 0, 0
			api, err := newAppStoreLocalAPIWith("darwin", true, []appStoreLocalAPIProfile{profile}, tc.version, []byte(tc.metadata),
				func() (int, string, error) { discoveries++; return appStoreTestPort, appStoreTestToken, nil },
				func(context.Context, string, string) (net.Conn, error) {
					dials++
					return nil, errors.New("unexpected dial")
				})
			if api != nil || err != errLocalAPIUnsupportedVersion {
				t.Fatalf("invalid profile constructed LocalAPI: api=%v err=%v", api, err)
			}
			if discoveries != 0 || dials != 0 {
				t.Fatalf("invalid metadata called discovery %d times and dial %d times", discoveries, dials)
			}
		})
	}

	discoveries := 0
	standalone, err := newLocalAPI(localAPITestVersion, []byte(sourceVersion))
	if err != nil {
		t.Fatalf("standalone pinned metadata refused: %v", err)
	}
	if standalone.client.OmitAuth != true || standalone.client.Dial == nil || standalone.client.Transport == nil {
		t.Fatal("standalone LocalAPI client did not retain its explicit authenticated socket policy")
	}
	if appStoreLocalAPIMetadataCandidate([]byte(sourceVersion)) || discoveries != 0 {
		t.Fatal("standalone metadata selected App Store transport")
	}

	for _, tc := range []struct {
		name       string
		statusBody string
		headerVer  string
	}{
		{name: "status body drift", statusBody: strings.Replace(sessionTestStatus, localAPITestVersion, "foreign-version", 1), headerVer: localAPITestVersion},
		{name: "response header drift", statusBody: sessionTestStatus, headerVer: "foreign-version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dial := httpPipeDialer(appStoreTestPort, func(request *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": {"application/json"}, "Tailscale-Version": {tc.headerVer}}
				return localAPIResponse(request, http.StatusOK, header, testBody(tc.statusBody)), nil
			})
			api, err := newAppStoreLocalAPIWith("darwin", true, []appStoreLocalAPIProfile{profile}, localAPITestVersion,
				appStoreTestMetadata(), syntheticAppStoreDiscovery(appStoreTestPort, appStoreTestToken), dial)
			if err != nil {
				t.Fatalf("construct synthetic App Store cell: %v", err)
			}
			if _, err := api.status(context.Background()); err != errLocalAPIResponse {
				t.Fatalf("response drift error = %v, want fixed response refusal", err)
			}
		})
	}
}

func TestAppStoreLocalAPIMissingOrDeniedAuthenticationFailsClosed(t *testing.T) {
	const expectedToken = "synthetic-required-token"
	for _, tc := range []struct {
		name        string
		clientToken string
		forcedCode  int
		wantErr     error
	}{
		{name: "missing header", forcedCode: http.StatusUnauthorized, wantErr: errLocalAPIResponse},
		{name: "wrong token", clientToken: "synthetic-wrong-token", forcedCode: http.StatusUnauthorized, wantErr: errLocalAPIResponse},
		{name: "401", clientToken: expectedToken, forcedCode: http.StatusUnauthorized, wantErr: errLocalAPIResponse},
		{name: "403", clientToken: expectedToken, forcedCode: http.StatusForbidden, wantErr: errLocalAPIAuthorization},
	} {
		t.Run(tc.name, func(t *testing.T) {
			discoveries := 0
			_, _, discoveryErr := boundedAppStoreDiscovery(func() (int, string, error) {
				discoveries++
				return appStoreTestPort, expectedToken, nil
			})
			if discoveryErr != nil || discoveries != 1 {
				t.Fatalf("synthetic discovery count=%d err=%v", discoveries, discoveryErr)
			}
			attempts := 0
			api := &localAPI{
				client: &local.Client{
					Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errLocalAPIRequest },
					Transport: noRedirectLocalAPITransport{
						next: localAPITestRoundTripper(func(request *http.Request) (*http.Response, error) {
							attempts++
							user, password, hasAuth := request.BasicAuth()
							if tc.clientToken == "" {
								if hasAuth || request.Header.Get("Authorization") != "" {
									t.Fatal("missing-auth fixture unexpectedly sent Authorization")
								}
							} else if !hasAuth || user != "" || password != tc.clientToken {
								t.Fatal("fixture sent an unexpected synthetic Basic credential")
							}
							return localAPIResponse(request, tc.forcedCode, localAPIJSONHeader(), testBody("private denial details")), nil
						}),
						basicAuthToken: localAPIAuthToken{value: tc.clientToken},
					},
					OmitAuth: true,
				},
				expectedVersion: localAPITestVersion,
			}
			_, err := api.status(context.Background())
			if err != tc.wantErr || attempts != 1 || discoveries != 1 {
				t.Fatalf("authorization result err=%v attempts=%d discoveries=%d", err, attempts, discoveries)
			}
			if strings.Contains(err.Error(), expectedToken) || (tc.clientToken != "" && strings.Contains(err.Error(), tc.clientToken)) || strings.Contains(err.Error(), "private denial") {
				t.Fatalf("authorization diagnostic leaked synthetic credential/detail: %v", err)
			}
		})
	}
}

func TestAppStoreLocalAPIRedactsDiscoveredSecret(t *testing.T) {
	const canaryToken = "canary-secret-value"
	const canaryPort = 43219
	logOutput := new(bytes.Buffer)
	originalOutput := log.Writer()
	log.SetOutput(logOutput)
	defer log.SetOutput(originalOutput)

	_, discoveryErr := newAppStoreLocalAPIWith("darwin", true, []appStoreLocalAPIProfile{appStoreTestProfile()}, localAPITestVersion,
		appStoreTestMetadata(), func() (int, string, error) {
			return canaryPort, canaryToken, fmt.Errorf("synthetic discovery failure %s %d", canaryToken, canaryPort)
		}, nil)
	if discoveryErr != errLocalAPIDiscovery {
		t.Fatalf("discovery error = %v", discoveryErr)
	}
	api, err := newAppStoreLocalAPIWith("darwin", true, []appStoreLocalAPIProfile{appStoreTestProfile()}, localAPITestVersion,
		appStoreTestMetadata(), syntheticAppStoreDiscovery(canaryPort, canaryToken),
		func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("synthetic dial failure %s %d", canaryToken, canaryPort)
		})
	if err != nil {
		t.Fatalf("construct redaction fixture: %v", err)
	}
	_, statusErr := api.status(context.Background())
	if statusErr != errLocalAPIRequest {
		t.Fatalf("status error = %v", statusErr)
	}
	_, watchErr := api.watch(context.Background())
	if watchErr != errLocalAPIRequest {
		t.Fatalf("watch error = %v", watchErr)
	}
	output := fmt.Sprint(api, api.client, api.client.Transport, discoveryErr, statusErr, watchErr, api.expectedVersion, logOutput.String(), fmt.Sprintf("%#v", localAPIAuthToken{value: canaryToken}))
	for _, secret := range []string{canaryToken, fmt.Sprint(canaryPort)} {
		if strings.Contains(output, secret) {
			t.Fatalf("diagnostic/string/log output exposed a synthetic secret or port")
		}
	}
}

func TestAppStoreLocalAPINeverUsesUpstreamDefaultAuthOrDiscovery(t *testing.T) {
	if appStoreLocalAPIProfileEnabled || len(appStoreLocalAPIProfiles) != 0 {
		t.Fatal("App Store transport must remain disabled with an empty production profile table")
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list package source for LocalAPI client audit: %v", err)
	}
	fset := token.NewFileSet()
	constructors := 0
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Client" {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "local" {
				return true
			}
			fields := map[string]ast.Expr{}
			for _, element := range literal.Elts {
				keyValue, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := keyValue.Key.(*ast.Ident)
				if ok {
					fields[key.Name] = keyValue.Value
				}
			}
			constructors++
			if fields["Dial"] == nil || fields["Transport"] == nil {
				t.Errorf("%s constructs local.Client without explicit Dial and Transport", file)
			}
			omit, ok := fields["OmitAuth"].(*ast.Ident)
			if !ok || omit.Name != "true" {
				t.Errorf("%s constructs local.Client without OmitAuth: true", file)
			}
			return true
		})
	}
	if constructors == 0 {
		t.Fatal("LocalAPI client construction audit found no constructors")
	}
}

func TestSessionAuthorityAppStoreFakeDaemonLifecycle(t *testing.T) {
	daemon := newSessionDaemon()
	wire := &appStoreWireRecorder{expectedToken: appStoreTestToken, daemon: daemon}
	discoveries := 0
	api, err := newAppStoreLocalAPIWith("darwin", true, []appStoreLocalAPIProfile{appStoreTestProfile()}, localAPITestVersion,
		appStoreTestMetadata(), func() (int, string, error) {
			discoveries++
			return appStoreTestPort, appStoreTestToken, nil
		}, wire.dial)
	if err != nil {
		t.Fatalf("construct synthetic App Store LocalAPI: %v", err)
	}
	authority, err := newSessionAuthority(api, 443, 8375)
	if err != nil {
		t.Fatalf("construct SessionAuthority: %v", err)
	}
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := authority.Activate(context.Background()); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if watches, posts := daemon.counts(); watches != 1 || posts != 1 {
		t.Fatalf("activation dispatches watches=%d posts=%d, want one each", watches, posts)
	}
	if err := authority.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	daemon.setConfig(addForeignConfig(daemon.currentConfig()))
	if err := authority.Retire(context.Background()); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	state := authority.Status()
	if !state.RouteCleared || !state.LocalWatchClosed || !state.RemoteWatchRetirementUnknown {
		t.Fatalf("retirement status lost required separation: %+v", state)
	}
	remaining := string(daemon.currentConfig())
	if strings.Contains(remaining, sessionTestID) || !strings.Contains(remaining, "independent-session") || !strings.Contains(remaining, "svc:foreign") || !strings.Contains(remaining, "AllowFunnel") {
		t.Fatalf("retirement did not selectively preserve foreign configuration: %s", remaining)
	}
	if watches, posts := daemon.counts(); watches != 1 || posts != 2 {
		t.Fatalf("complete lifecycle dispatches watches=%d posts=%d, want one watch and two conditional writes", watches, posts)
	}
	if discoveries != 1 {
		t.Fatalf("App Store discovery ran %d times for one constructed client", discoveries)
	}
	wire.mu.Lock()
	defer wire.mu.Unlock()
	if wire.badAuth || wire.watchAuthorization == "" || wire.watchAuthorization != wire.expectedAuthorization {
		t.Fatal("watch did not carry the same synthetic Basic credential as other LocalAPI requests")
	}
	if wire.connections < 4 {
		t.Fatalf("fake daemon saw only %d in-memory connections", wire.connections)
	}
}

func TestLocalAPIStandaloneSocketPolicyUnchanged(t *testing.T) {
	for file, socket := range map[string]string{
		"localapi_transport_darwin.go": "/var/run/tailscaled.socket",
		"localapi_transport_linux.go":  "/var/run/tailscale/tailscaled.sock",
	} {
		source, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(source), socket) {
			t.Errorf("standalone socket policy missing from %s", file)
		}
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		api, err := newLocalAPI(localAPITestVersion, []byte(sourceVersion))
		if err != nil {
			t.Fatalf("construct standalone LocalAPI: %v", err)
		}
		if api.client.OmitAuth != true || api.client.Dial == nil || api.client.Transport == nil {
			t.Fatal("standalone transport lost its OmitAuth or explicit socket dial policy")
		}
	} else if _, err := newLocalAPI(localAPITestVersion, []byte(sourceVersion)); err == nil {
		t.Fatal("unsupported runtime constructed standalone LocalAPI")
	}
	for _, goos := range []string{"android", "ios", "freebsd", "windows"} {
		if err := checkLocalAPIRuntime(goos); err == nil {
			t.Errorf("unsupported runtime %q was admitted", goos)
		}
	}
}

type appStoreWireRecorder struct {
	mu                    sync.Mutex
	expectedToken         string
	expectedAuthorization string
	watchAuthorization    string
	connections           int
	badAuth               bool
	daemon                *sessionDaemon
}

func (w *appStoreWireRecorder) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil || network != "tcp" || address != "127.0.0.1:43123" {
		return nil, errors.New("in-memory dial target refused")
	}
	client, peer := net.Pipe()
	w.mu.Lock()
	w.connections++
	w.mu.Unlock()
	go func() {
		defer peer.Close()
		request, err := http.ReadRequest(bufio.NewReader(peer))
		if err != nil {
			return
		}
		username, password, authenticated := request.BasicAuth()
		authorization := request.Header.Get("Authorization")
		w.mu.Lock()
		if !authenticated || username != "" || password != w.expectedToken {
			w.badAuth = true
		}
		if request.URL.Path == localAPIWatchPath {
			w.watchAuthorization = authorization
		}
		if w.expectedAuthorization == "" {
			w.expectedAuthorization = authorization
		}
		if authorization != w.expectedAuthorization {
			w.badAuth = true
		}
		w.mu.Unlock()
		response, err := w.daemon.RoundTrip(request)
		if err != nil || response == nil {
			return
		}
		response.Request = request
		if response.Body != nil {
			response.ContentLength = -1
			go func(body io.ReadCloser) {
				_, _ = io.Copy(io.Discard, peer)
				_ = body.Close()
			}(response.Body)
		}
		_ = response.Write(peer)
		if response.Body != nil {
			_ = response.Body.Close()
		}
	}()
	return client, nil
}

func httpPipeDialer(port int, handler func(*http.Request) (*http.Response, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if ctx == nil || network != "tcp" || address != fmt.Sprintf("127.0.0.1:%d", port) {
			return nil, errors.New("in-memory HTTP dial target refused")
		}
		client, peer := net.Pipe()
		go func() {
			defer peer.Close()
			request, err := http.ReadRequest(bufio.NewReader(peer))
			if err != nil {
				return
			}
			response, err := handler(request)
			if err != nil || response == nil {
				return
			}
			response.Request = request
			if response.Body != nil {
				response.ContentLength = -1
			}
			_ = response.Write(peer)
			if response.Body != nil {
				_ = response.Body.Close()
			}
		}()
		return client, nil
	}
}
