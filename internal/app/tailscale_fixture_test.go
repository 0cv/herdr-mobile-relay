//go:build herdr_tailscale_test

package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
	"github.com/0cv/herdr-mobile-relay/internal/web"
)

const (
	managedFixtureVersion  = "0.9.0"
	managedFixtureRevision = "test-revision"
	managedFixtureHost     = "relay.tailnet.ts.net"
	managedFixtureInstance = "integration-instance"
	managedFixtureRun      = "integration-run"
)

const managedFixtureStatus = `{"Version":"1.102.4-tbbcd7d1fc","BackendState":"Running","Self":{"ID":"node-1","UserID":123,"DNSName":"relay.tailnet.ts.net.","CapMap":{"https":null}},"CurrentTailnet":{"Name":"example-account","MagicDNSSuffix":"tailnet.ts.net","MagicDNSEnabled":true},"CertDomains":["relay.tailnet.ts.net"],"User":{"123":{"ID":123,"LoginName":"user@example.invalid","DisplayName":"Example","ProfilePicURL":""}}}`

// appLocalAPIFake is a byte-level protocol fake for the production
// SessionAuthority. It accepts only the fixed LocalAPI host and paths and
// models the pinned conditional Serve-config API; it never supplies ownership
// or readiness booleans.
type appLocalAPIFake struct {
	mu sync.Mutex

	status            []byte
	config            []byte
	revision          uint64
	statusCalls       int
	watchCalls        int
	postCalls         int
	watchQuery        string
	watchWriter       *io.PipeWriter
	registrationValid bool
	events            []string
}

func newAppLocalAPIFake() *appLocalAPIFake {
	return &appLocalAPIFake{status: []byte(managedFixtureStatus), config: []byte("null"), revision: 1}
}

func (f *appLocalAPIFake) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "http" || request.URL.Host != "local-tailscaled.sock" {
		return nil, fmt.Errorf("unexpected LocalAPI target %s", request.URL)
	}
	if request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" {
		return nil, errors.New("LocalAPI request unexpectedly carried caller-supplied authorization")
	}
	f.mu.Lock()
	switch request.URL.Path {
	case "/localapi/v0/status":
		f.statusCalls++
		body := append([]byte(nil), f.status...)
		f.events = append(f.events, "status")
		f.mu.Unlock()
		return appLocalAPIResponse(request, http.StatusOK, appLocalAPIHeader(), io.NopCloser(bytes.NewReader(body))), nil
	case "/localapi/v0/serve-config":
		if request.Method == http.MethodGet {
			body := append([]byte(nil), f.config...)
			header := appLocalAPIHeader()
			header.Set("ETag", f.etagLocked())
			f.events = append(f.events, "config:get")
			f.mu.Unlock()
			return appLocalAPIResponse(request, http.StatusOK, header, io.NopCloser(bytes.NewReader(body))), nil
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
		f.postCalls++
		f.events = append(f.events, "config:post")
		if f.postCalls == 1 {
			var document struct {
				Foreground map[string]json.RawMessage `json:"Foreground"`
			}
			f.registrationValid = f.watchCalls == 1 && json.Unmarshal(body, &document) == nil && document.Foreground["hosted-fixture-watch"] != nil
			if !f.registrationValid {
				f.mu.Unlock()
				return appLocalAPIResponse(request, http.StatusBadRequest, appLocalAPIHeader(), io.NopCloser(strings.NewReader("invalid fixture registration"))), nil
			}
		}
		if request.Header.Get("If-Match") != f.etagLocked() {
			f.mu.Unlock()
			header := http.Header{"Tailscale-Version": {"1.102.4-tbbcd7d1fc"}, "Content-Type": {"text/plain; charset=utf-8"}}
			return appLocalAPIResponse(request, http.StatusPreconditionFailed, header, io.NopCloser(strings.NewReader("etag mismatch\n"))), nil
		}
		f.config = append([]byte(nil), body...)
		f.revision++
		f.mu.Unlock()
		return appLocalAPIResponse(request, http.StatusOK, appLocalAPIHeader(), io.NopCloser(strings.NewReader(""))), nil
	case "/localapi/v0/watch-ipn-bus":
		f.watchCalls++
		f.watchQuery = request.URL.RawQuery
		f.events = append(f.events, "watch")
		if f.watchQuery != "mask=2" {
			f.mu.Unlock()
			return appLocalAPIResponse(request, http.StatusBadRequest, appLocalAPIHeader(), io.NopCloser(strings.NewReader("bad mask\n"))), nil
		}
		reader, writer := io.Pipe()
		f.watchWriter = writer
		f.mu.Unlock()
		go func() {
			_, _ = io.WriteString(writer, `{"Version":"1.102.4-tbbcd7d1fc","SessionID":"hosted-fixture-watch"}`+"\n")
			<-request.Context().Done()
			_ = writer.Close()
		}()
		return appLocalAPIResponse(request, http.StatusOK, appLocalAPIHeader(), reader), nil
	default:
		f.mu.Unlock()
		return nil, fmt.Errorf("unexpected LocalAPI path %s", request.URL.Path)
	}
}

func appLocalAPIResponse(request *http.Request, status int, header http.Header, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       body,
		Request:    request,
	}
}

func appLocalAPIHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}, "Tailscale-Version": {"1.102.4-tbbcd7d1fc"}}
}

func (f *appLocalAPIFake) etagLocked() string { return fmt.Sprintf("%064x", f.revision) }

func (f *appLocalAPIFake) driftIdentityNow() {
	f.mu.Lock()
	f.status = bytes.Replace(f.status, []byte(`"ID":"node-1"`), []byte(`"ID":"node-2"`), 1)
	f.mu.Unlock()
}

func (f *appLocalAPIFake) restoreIdentity() {
	f.mu.Lock()
	f.status = []byte(managedFixtureStatus)
	f.mu.Unlock()
}

func (f *appLocalAPIFake) endWatch() {
	f.mu.Lock()
	writer := f.watchWriter
	f.mu.Unlock()
	if writer != nil {
		_ = writer.Close()
	}
}

func (f *appLocalAPIFake) snapshot() (watchCount, postCount int, watchQuery string, registrationValid bool, events []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchCalls, f.postCalls, f.watchQuery, f.registrationValid, append([]string(nil), f.events...)
}

type managedTailscaleFixture struct {
	t *testing.T

	root                            string
	webRoot                         string
	owner                           *ManagedOwner
	server                          *Server
	localAPI                        *appLocalAPIFake
	backend                         *http.Server
	backendLn                       net.Listener
	public                          *http.Server
	publicLn                        net.Listener
	control                         *localcontrol.Server
	controlCallbacks                chan string
	controlCtx                      context.Context
	cancelCtrl                      context.CancelFunc
	origin                          string
	hostPort                        string
	rootCerts                       *x509.CertPool
	backendMu                       sync.Mutex
	readyCalls                      int
	failReadyAt                     int
	publicMu                        sync.Mutex
	publicHealthCalls               int
	failPublicHealthAt              int
	versionCalls                    int
	tamperVersionAt                 int
	versionDelayAt                  int
	versionDelayStarted             chan struct{}
	versionDelayRelease             chan struct{}
	driftIdentityAtVersion          int
	deviceStoreAppearedDuringPublic bool
}

func newManagedTailscaleFixture(t *testing.T) *managedTailscaleFixture {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "htr-it-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := &managedTailscaleFixture{t: t, root: root, localAPI: newAppLocalAPIFake(), controlCallbacks: make(chan string, 8)}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	owner, err := AcquireManagedOwner(root)
	if err != nil {
		t.Fatalf("AcquireManagedOwner: %v", err)
	}
	fixture.owner = owner

	fixture.webRoot = filepath.Join(root, "web")
	writeManagedWebFixture(t, fixture.webRoot, managedFixtureVersion, managedFixtureRevision)
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.backendLn = backendLn
	publicTCP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = backendLn.Close()
		t.Fatal(err)
	}
	fixture.publicLn = publicTCP
	_, portText, err := net.SplitHostPort(publicTCP.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	origin := "https://" + managedFixtureHost + ":" + portText
	fixture.origin = origin
	fixture.hostPort = managedFixtureHost + ":" + portText

	cfg := &config.Config{
		Host:              "127.0.0.1",
		Port:              portFromAddr(t, backendLn.Addr()),
		PluginPort:        0,
		SocketPath:        filepath.Join(root, "herdr.sock"),
		PollInterval:      3600,
		RuntimeDir:        root,
		CacheDir:          filepath.Join(root, "cache"),
		ConfigHome:        filepath.Join(root, "config"),
		WebRoot:           fixture.webRoot,
		HerdrBin:          filepath.Join(root, "missing-herdr"),
		Token:             strings.Repeat("k", 32),
		Transport:         config.TransportTailscale,
		TailscaleOrigin:   origin,
		InstanceID:        managedFixtureInstance,
		ManagedRunID:      managedFixtureRun,
		PairingSocketPath: filepath.Join(root, "control.sock"),
	}
	authority, err := tailscale.NewSessionAuthorityWithRawLocalAPIForTest(fixture.localAPI, mustPort(t, origin), cfg.Port)
	if err != nil {
		t.Fatalf("construct real SessionAuthority over raw LocalAPI fake: %v", err)
	}
	if err := authority.Prepare(context.Background()); err != nil {
		t.Fatalf("SessionAuthority.Prepare: %v", err)
	}
	preparedOrigin, ok := authority.Origin()
	if !ok || preparedOrigin != origin {
		t.Fatalf("prepared origin = %q, %t; want %q", preparedOrigin, ok, origin)
	}
	fixture.server = newServerWithSession(cfg, managedFixtureVersion, managedFixtureRevision, managedTestLogger(), owner, authority)
	webHandler, err := web.NewHandler(fixture.webRoot)
	if err != nil {
		t.Fatalf("web.NewHandler: %v", err)
	}
	fixture.server.webH = webHandler
	fixture.server.state.CommitInventory(nil, fixture.server.state.RevisionCounter())
	udp, err := coordinator.NewUDPListener("127.0.0.1:0", fixture.server.state, cfg.SocketPath, managedTestLogger())
	if err != nil {
		t.Fatalf("bind real fixture UDP listener: %v", err)
	}
	fixture.server.udp = udp
	fixture.server.mu.Lock()
	fixture.server.ready = true
	fixture.server.backendBound = true
	fixture.server.mu.Unlock()
	fixture.backend = &http.Server{Handler: http.HandlerFunc(fixture.serveBackend)}
	go func() { _ = fixture.backend.Serve(backendLn) }()

	publicURL, _ := url.Parse("http://" + backendLn.Addr().String())
	proxy := httputil.NewSingleHostReverseProxy(publicURL)
	proxy.Transport = &http.Transport{Proxy: nil, DisableKeepAlives: true}
	fixture.public = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.servePublic(proxy, w, r)
	})}
	certificate, roots := managedFixtureCertificate(t, managedFixtureHost)
	fixture.rootCerts = roots
	tlsListener := tls.NewListener(publicTCP, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
	go func() { _ = fixture.public.Serve(tlsListener) }()
	installManagedHealthTestNetwork(t, fixture.server, fixture.hostPort, publicTCP.Addr().String(), roots)
	return fixture
}

func (f *managedTailscaleFixture) serveBackend(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/readyz" {
		f.backendMu.Lock()
		f.readyCalls++
		fail := f.failReadyAt != 0 && f.readyCalls == f.failReadyAt
		f.backendMu.Unlock()
		if fail {
			http.Error(w, "fixture readiness failure", http.StatusServiceUnavailable)
			return
		}
	}
	f.server.httpHandler().ServeHTTP(w, r)
}

func (f *managedTailscaleFixture) servePublic(proxy *httputil.ReverseProxy, w http.ResponseWriter, r *http.Request) {
	if _, err := os.Lstat(filepath.Join(f.root, "device-auth")); err == nil {
		f.publicMu.Lock()
		f.deviceStoreAppearedDuringPublic = true
		f.publicMu.Unlock()
	}
	if r.URL.Path == "/healthz" {
		f.publicMu.Lock()
		f.publicHealthCalls++
		fail := f.failPublicHealthAt != 0 && f.publicHealthCalls == f.failPublicHealthAt
		f.publicMu.Unlock()
		if fail {
			http.Error(w, "fixture public health failure", http.StatusServiceUnavailable)
			return
		}
	}
	if r.URL.Path == "/version.json" {
		f.publicMu.Lock()
		f.versionCalls++
		tamper := f.tamperVersionAt != 0 && f.versionCalls == f.tamperVersionAt
		driftIdentity := f.driftIdentityAtVersion != 0 && f.versionCalls == f.driftIdentityAtVersion
		delay := f.versionDelayAt != 0 && f.versionCalls == f.versionDelayAt
		started, release := f.versionDelayStarted, f.versionDelayRelease
		if delay {
			f.versionDelayAt = 0
		}
		if driftIdentity {
			f.driftIdentityAtVersion = 0
		}
		f.publicMu.Unlock()
		if delay {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
		if driftIdentity {
			f.localAPI.driftIdentityNow()
		}
		if tamper {
			path := filepath.Join(f.webRoot, "version.json")
			data, err := os.ReadFile(path)
			if err == nil {
				data = bytes.Replace(data, []byte(managedFixtureRevision), []byte("changed-revision"), 1)
				_ = os.WriteFile(path, data, 0o644)
			}
		}
		return
	}
	proxy.ServeHTTP(w, r)
}

func (f *managedTailscaleFixture) failAfterNextArmReadinessPass(local, public bool) {
	f.backendMu.Lock()
	if local {
		f.failReadyAt = f.readyCalls + 2
	}
	f.backendMu.Unlock()
	f.publicMu.Lock()
	if public {
		f.failPublicHealthAt = f.publicHealthCalls + 2
	}
	f.publicMu.Unlock()
}

func (f *managedTailscaleFixture) delayPublicVersionAfter(additionalRequests int) (<-chan struct{}, func()) {
	f.publicMu.Lock()
	f.versionDelayAt = f.versionCalls + additionalRequests
	started := make(chan struct{})
	release := make(chan struct{})
	f.versionDelayStarted = started
	f.versionDelayRelease = release
	f.publicMu.Unlock()
	var releaseOnce sync.Once
	return started, func() { releaseOnce.Do(func() { close(release) }) }
}

func (f *managedTailscaleFixture) tamperBundleAfterArmPreflight() {
	f.publicMu.Lock()
	f.tamperVersionAt = f.versionCalls + 3
	f.publicMu.Unlock()
}

func (f *managedTailscaleFixture) driftOwnerAfterArmPreflight() {
	f.publicMu.Lock()
	f.driftIdentityAtVersion = f.versionCalls + 3
	f.publicMu.Unlock()
}

func (f *managedTailscaleFixture) startControl(arm func(context.Context) (localcontrol.Status, error)) {
	f.t.Helper()
	if arm == nil {
		arm = f.server.armForControl
	}
	callbacks := localcontrol.Callbacks{
		Status: f.server.pairingControlStatusContext,
		Activate: func(ctx context.Context) (localcontrol.Status, error) {
			defer f.noteControlCallback("activate")
			return f.server.activateForControl(ctx)
		},
		Arm: func(ctx context.Context) (localcontrol.Status, error) {
			defer f.noteControlCallback("arm_bootstrap")
			return arm(ctx)
		},
		Retire: func(ctx context.Context) (localcontrol.Status, error) {
			defer f.noteControlCallback("retire")
			return f.server.retireForControl(ctx)
		},
		Retired: f.server.CompleteManagedTailscaleRetirement,
	}
	control, err := localcontrol.NewManaged(f.server.cfg.PairingSocketPath, managedFixtureRun, managedFixtureInstance, callbacks)
	if err != nil {
		f.t.Fatalf("localcontrol.NewManaged: %v", err)
	}
	f.control = control
	f.controlCtx, f.cancelCtrl = context.WithCancel(context.Background())
	go func() { _ = control.Run(f.controlCtx) }()
	f.t.Cleanup(func() {
		if f.server != nil && !f.server.ManagedOwnerReleaseSafe() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = f.server.RetireManagedTailscale(ctx)
			cancel()
		}
		if f.cancelCtrl != nil {
			f.cancelCtrl()
		}
		if f.control != nil {
			_ = f.control.Close()
		}
		if f.public != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = f.public.Shutdown(ctx)
			cancel()
		}
		if f.backend != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = f.backend.Shutdown(ctx)
			cancel()
		}
		if f.publicLn != nil {
			_ = f.publicLn.Close()
		}
		if f.backendLn != nil {
			_ = f.backendLn.Close()
		}
		if f.server != nil {
			if f.server.udp != nil {
				_ = f.server.udp.Close()
			}
			if f.server.hub != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = f.server.hub.Shutdown(ctx)
				cancel()
			}
			if f.server.webH != nil {
				_ = f.server.webH.Close()
			}
			if f.server.herdrC != nil {
				_ = f.server.herdrC.Close()
			}
		}
		if f.owner != nil {
			if f.server != nil && f.server.ManagedOwnerReleaseSafe() {
				_ = RetireManagedOwner(f.owner, nil)
			} else {
				_ = f.owner.Close()
			}
		}
	})
}

func (f *managedTailscaleFixture) noteControlCallback(operation string) {
	select {
	case f.controlCallbacks <- operation:
	default:
	}
}

func (f *managedTailscaleFixture) waitControlCallback(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-f.controlCallbacks:
		if got != want {
			t.Fatalf("completed control callback = %q, want %q", got, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s control callback completion", want)
	}
}

func (f *managedTailscaleFixture) activate(t *testing.T) localcontrol.Response {
	t.Helper()
	response, err := localcontrol.Request(context.Background(), f.server.cfg.PairingSocketPath, "activate", managedFixtureRun, managedFixtureInstance)
	if err != nil {
		t.Fatalf("localcontrol activate: %v", err)
	}
	f.waitControlCallback(t, "activate")
	return response
}

func (f *managedTailscaleFixture) arm(t *testing.T) (localcontrol.Response, error) {
	t.Helper()
	response, err := localcontrol.Request(context.Background(), f.server.cfg.PairingSocketPath, "arm_bootstrap", managedFixtureRun, managedFixtureInstance)
	f.waitControlCallback(t, "arm_bootstrap")
	return response, err
}

func (f *managedTailscaleFixture) localReadyCount() int {
	f.backendMu.Lock()
	defer f.backendMu.Unlock()
	return f.readyCalls
}

func (f *managedTailscaleFixture) publicHealthCount() int {
	f.publicMu.Lock()
	defer f.publicMu.Unlock()
	return f.publicHealthCalls
}

func writeManagedWebFixture(t *testing.T, root, version, revision string) {
	t.Helper()
	javascript := []byte("console.log('hosted managed fixture');\n")
	stylesheet := []byte("body { color: black; }\n")
	javascriptPath := "assets/app-" + managedDigest(javascript) + ".js"
	stylesheetPath := "assets/app-" + managedDigest(stylesheet) + ".css"
	entryPath := "builds/" + version + "-1-aaaaaaaaaaaaaaaa/index.html"
	entry := []byte(`<!doctype html><html><head><link rel="stylesheet" href="/` + stylesheetPath + `" integrity="` + managedIntegrity(stylesheet) + `" crossorigin="anonymous"></head><body><script src="/` + javascriptPath + `" integrity="` + managedIntegrity(javascript) + `" crossorigin="anonymous"></script></body></html>`)
	for name, data := range map[string][]byte{javascriptPath: javascript, stylesheetPath: stylesheet, entryPath: entry} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	build := strings.Repeat("a", 64)
	descriptor := release.WebDescriptor{
		Schema: release.WebDescriptorSchema, Version: version, Assets: 1, Build: build, Entry: "/" + entryPath,
		Files: map[string]release.WebDescriptorFile{
			"entry":      {Path: entryPath, SHA256: managedDigest(entry), Integrity: managedIntegrity(entry)},
			"javascript": {Path: javascriptPath, SHA256: managedDigest(javascript), Integrity: managedIntegrity(javascript)},
			"stylesheet": {Path: stylesheetPath, SHA256: managedDigest(stylesheet), Integrity: managedIntegrity(stylesheet)},
		},
	}
	writeManagedJSON(t, filepath.Join(root, "release.json"), descriptor)
	writeManagedJSON(t, filepath.Join(root, "version.json"), map[string]any{
		"version": version, "release_version": version, "revision": revision, "assets": 1, "build": build,
		"entry": descriptor.Entry, "script": "/" + javascriptPath, "style": "/" + stylesheetPath,
		"script_sha256": descriptor.Files["javascript"].SHA256, "style_sha256": descriptor.Files["stylesheet"].SHA256,
	})
}

func writeManagedJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func managedDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func managedIntegrity(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func managedFixtureCertificate(t *testing.T, hostname string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Herdr isolated fixture CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		BasicConstraintsValid: true, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(leafPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return certificate, roots
}

var managedHealthTestNetwork struct {
	mu          sync.RWMutex
	server      *Server
	hostPort    string
	dialAddress string
	roots       *x509.CertPool
}

// This replacement exists only in the explicitly tagged app test binary. The
// production build uses managed_listener.go's fixed trust-store client.
func managedHealthClientForServer(target *Server, timeout time.Duration) *http.Client {
	managedHealthTestNetwork.mu.RLock()
	server, hostPort, dialAddress, roots := managedHealthTestNetwork.server,
		managedHealthTestNetwork.hostPort, managedHealthTestNetwork.dialAddress, managedHealthTestNetwork.roots
	managedHealthTestNetwork.mu.RUnlock()
	if target != server || roots == nil {
		return managedHealthClient(timeout)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
			DialContext:     managedFixtureDial(hostPort, dialAddress),
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func installManagedHealthTestNetwork(t *testing.T, server *Server, hostPort, dialAddress string, roots *x509.CertPool) {
	t.Helper()
	managedHealthTestNetwork.mu.Lock()
	managedHealthTestNetwork.server = server
	managedHealthTestNetwork.hostPort = hostPort
	managedHealthTestNetwork.dialAddress = dialAddress
	managedHealthTestNetwork.roots = roots
	managedHealthTestNetwork.mu.Unlock()
	previousDefaultTransport := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		Proxy: nil, DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext:     managedFixtureDial(hostPort, dialAddress),
	}
	t.Cleanup(func() {
		managedHealthTestNetwork.mu.Lock()
		managedHealthTestNetwork.server = nil
		managedHealthTestNetwork.hostPort = ""
		managedHealthTestNetwork.dialAddress = ""
		managedHealthTestNetwork.roots = nil
		managedHealthTestNetwork.mu.Unlock()
		http.DefaultTransport = previousDefaultTransport
	})
}

func managedFixtureDial(hostPort, dialAddress string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == hostPort {
			return (&net.Dialer{}).DialContext(ctx, network, dialAddress)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

func portFromAddr(t *testing.T, address net.Addr) int {
	t.Helper()
	port, err := mustAddrPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func mustPort(t *testing.T, origin string) int {
	t.Helper()
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	port := parsed.Port()
	var result int
	if _, err := fmt.Sscanf(port, "%d", &result); err != nil || result < 1 || result > 65535 {
		t.Fatalf("invalid fixture HTTPS port %q", port)
	}
	return result
}

func mustAddrPort(address net.Addr) (int, error) {
	_, portText, err := net.SplitHostPort(address.String())
	if err != nil {
		return 0, err
	}
	var port int
	_, err = fmt.Sscanf(portText, "%d", &port)
	return port, err
}
