package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/doctor"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/release"
)

func diagnosticFixture(t *testing.T, text string) (diagnosticRuntime, string, doctor.BuildIdentity) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "private-config-canary")
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("web/index.html\x00" + strings.Repeat("a", 64) + "\n"))
	build := doctor.BuildIdentity{Version: "0.22.4-397", Revision: strings.Repeat("b", 40), Target: release.CurrentTarget(), WebHash: hex.EncodeToString(hash[:])}
	manifest := map[string]any{"schema": 1, "version": build.Version, "revision": build.Revision, "target": build.Target, "web_hash": build.WebHash,
		"files": map[string]string{"web/index.html": strings.Repeat("a", 64), "herdr-mobile-relay": strings.Repeat("c", 64)}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), data, 0o444); err != nil {
		t.Fatal(err)
	}
	// A regular executable-shaped file for path resolution only; NEVER run it.
	// Its bytes deliberately do not match the manifest's binary hash: metadata
	// acquisition still cannot claim binary or complete package integrity.
	if err := os.WriteFile(filepath.Join(root, "herdr-mobile-relay"), []byte("inert executable-path fixture\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	runtime := productionDiagnosticRuntime()
	runtime.executable = func() (string, error) { return filepath.Join(root, "herdr-mobile-relay"), nil }
	runtime.version, runtime.revision, runtime.target = build.Version, build.Revision, build.Target
	runtime.getenv = func(key string) string {
		if key == "HERDR_RELAY_ENV" {
			return config
		}
		if key == "HERDR_PLUGIN_CONFIG_DIR" {
			return ""
		}
		t.Fatal("diagnostic read ambient routing/credential/build authority")
		return ""
	}
	installDarwinSupportedHostFixtures(&runtime, root, data, build)
	return runtime, config, build
}

// Darwin command fixtures that exercise later report/query composition use an
// injected supported-host model. Dedicated reader tests cover actual Darwin
// refusal; no fixture here changes ACLs or represents native ACL qualification.
func installDarwinSupportedHostFixtures(runtime *diagnosticRuntime, root string, manifest []byte, build doctor.BuildIdentity) {
	if goruntime.GOOS != "darwin" {
		return
	}
	runtime.readConfig = func(path string) doctor.Inspection {
		info, err := os.Lstat(path)
		if err != nil {
			return doctor.Inspection{}
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 {
			return doctor.ParseConfig(strings.NewReader("!"))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return doctor.Inspection{}
		}
		return doctor.ParseConfig(bytes.NewReader(data))
	}
	runtime.readIdentity = func(path, version, revision, target string) doctor.BuildIdentity {
		if path != root || version != build.Version || revision != build.Revision || target != build.Target {
			return doctor.BuildIdentity{}
		}
		info, err := os.Lstat(filepath.Join(root, "release-manifest.json"))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return doctor.BuildIdentity{}
		}
		data, err := os.ReadFile(filepath.Join(root, "release-manifest.json"))
		if err != nil || !bytes.Equal(data, manifest) {
			return doctor.BuildIdentity{}
		}
		return build
	}
}

// Match the installer topology: ~/.local/bin/herdr-mobile-relay points through
// release_root/current to a package binary. Only disposable symlinks/files;
// the executable-shaped fixture is never invoked.
func diagnosticInstalledShim(t *testing.T, packageRoot string) (string, string, string) {
	t.Helper()
	layout, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(layout, ".local", "bin")
	releaseRoot := filepath.Join(layout, ".local", "share", "herdr-mobile-relay")
	for _, dir := range []string{shimDir, releaseRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(releaseRoot, "current")
	if err := os.Symlink(packageRoot, current); err != nil {
		t.Fatal(err)
	}
	currentBinary := filepath.Join(current, "herdr-mobile-relay")
	shim := filepath.Join(shimDir, "herdr-mobile-relay")
	if err := os.Symlink(currentBinary, shim); err != nil {
		t.Fatal(err)
	}
	return shim, currentBinary, layout
}

func TestDiagnosticResolvesInstalledExecutableBeforePackageSelection(t *testing.T) {
	for _, kind := range []string{"direct", "shim", "current"} {
		t.Run(kind, func(t *testing.T) {
			runtime, selected, build := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
			root := filepath.Dir(selected)
			if err := os.Mkdir(filepath.Join(root, "relay"), 0o700); err != nil {
				t.Fatal(err)
			}
			packagedConfig := filepath.Join(root, "relay", ".env")
			if err := os.WriteFile(packagedConfig, []byte("HERDR_RELAY_TRANSPORT=cloudflare\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			shim, current, layout := diagnosticInstalledShim(t, root)
			executable := filepath.Join(root, "herdr-mobile-relay")
			if kind == "shim" {
				executable = shim
			}
			if kind == "current" {
				executable = current
			}
			runtime.executable = func() (string, error) { return executable, nil }
			runtime.getenv = func(key string) string {
				if key != "HERDR_RELAY_ENV" && key != "HERDR_PLUGIN_CONFIG_DIR" {
					t.Fatal("unexpected ambient authority")
				}
				return ""
			}
			configReads, metadataReads, factories := 0, 0, 0
			originalConfig, originalMetadata, originalQueries := runtime.readConfig, runtime.readIdentity, runtime.queries
			runtime.readConfig = func(path string) doctor.Inspection {
				configReads++
				if path != packagedConfig {
					t.Fatal("default config selected beside shim/current instead of resolved package")
				}
				return originalConfig(path)
			}
			runtime.readIdentity = func(path, version, revision, target string) doctor.BuildIdentity {
				metadataReads++
				if path != root || version != build.Version || revision != build.Revision || target != build.Target {
					t.Fatal("metadata authority changed during executable resolution")
				}
				return originalMetadata(path, version, revision, target)
			}
			runtime.queries = func(physical string, cfg doctor.Inspection, got doctor.BuildIdentity) doctor.Dependencies {
				factories++
				if physical != root || got != build {
					t.Fatal("resolved package metadata failed to bind actual package/local adapters")
				}
				return originalQueries(physical, cfg, got)
			}
			beforePackage, beforeLayout := diagnosticInventory(t, root), diagnosticInventory(t, layout)
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(context.Background(), []string{"--json"}, false, &out, runtime)
			if err != nil || code != 3 || configReads != 1 || metadataReads != 1 || factories != 1 {
				t.Fatal("installed path did not compose one partial report")
			}
			doc := diagnosticDocument(t, out.String())
			if doc["relay_build"].(map[string]any)["web_hash"] != build.WebHash {
				t.Fatal("resolved expected metadata absent from report")
			}
			for _, item := range doc["checks"].([]any) {
				check := item.(map[string]any)
				if check["id"] == string(doctor.CheckBundle) && (check["state"] != "unknown" || check["required"] != true) {
					t.Fatal("resolving executable claimed package integrity")
				}
			}
			if diagnosticInventory(t, root) != beforePackage || diagnosticInventory(t, layout) != beforeLayout {
				t.Fatal("executable resolution mutated package/shim/config")
			}
			diagnosticPrivacy(t, out.String())
		})
	}
}

func TestDiagnosticUnresolvableExecutableDoesNotGuessMetadata(t *testing.T) {
	for _, kind := range []string{"empty", "relative", "unclean", "missing", "dangling", "cycle", "directory", "nonexecutable"} {
		t.Run(kind, func(t *testing.T) {
			runtime, selected, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\nHERDR_RELAY_INSTANCE_ID=private-instance-canary\n")
			root := filepath.Dir(selected)
			path := filepath.Join(root, "private-executable-canary")
			switch kind {
			case "empty":
				path = ""
			case "relative":
				path = "private-executable-canary"
			case "unclean":
				path = root + "/../private-executable-canary"
			case "dangling":
				if err := os.Symlink(filepath.Join(root, "absent"), path); err != nil {
					t.Fatal(err)
				}
			case "cycle":
				if err := os.Symlink(path, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "nonexecutable":
				if err := os.WriteFile(path, []byte("never executed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runtime.executable = func() (string, error) { return path, nil }
			runtime.readIdentity = func(string, string, string, string) doctor.BuildIdentity {
				t.Fatal("invalid executable guessed package metadata")
				return doctor.BuildIdentity{}
			}
			originalQueries := runtime.queries
			runtime.queries = func(physical string, cfg doctor.Inspection, build doctor.BuildIdentity) doctor.Dependencies {
				if physical != "" || build != (doctor.BuildIdentity{}) {
					t.Fatal("invalid executable manufactured identity")
				}
				return originalQueries(physical, cfg, build)
			}
			before := diagnosticInventory(t, root)
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(context.Background(), []string{"--json", "--config", selected}, false, &out, runtime)
			if err != nil || code != 3 || diagnosticDocument(t, out.String())["relay_build"].(map[string]any)["version"] != "unknown" {
				t.Fatal("unresolved executable did not remain incomplete")
			}
			if diagnosticInventory(t, root) != before {
				t.Fatal("invalid executable resolution mutated state")
			}
			diagnosticPrivacy(t, out.String())
		})
	}
}

func diagnosticCheck(t *testing.T, report doctor.Report, id doctor.CheckID) doctor.Check {
	t.Helper()
	for _, c := range report.Checks() {
		if c.ID == id {
			return c
		}
	}
	t.Fatal("missing required check")
	return doctor.Check{}
}
func diagnosticDocument(t *testing.T, text string) map[string]any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(text))
	var value map[string]any
	if err := d.Decode(&value); err != nil {
		t.Fatal("missing finalized JSON document", err)
	}
	if _, err := d.Token(); err != io.EOF {
		t.Fatal("JSON contaminated with another document/banner")
	}
	if value["schema_version"] != float64(1) {
		t.Fatal("wrong schema")
	}
	return value
}
func diagnosticPrivacy(t *testing.T, text string) {
	t.Helper()
	for _, canary := range []string{"private-config-canary", "private-executable-canary", "private-instance-canary", "private-node-canary", "secret-command-canary", "secret-output-canary", "private-error-canary", "https://private", "wss://private", "$(", "\x1b"} {
		if strings.Contains(text, canary) {
			t.Fatal("diagnostic input/error escaped finite output")
		}
	}
}

// Full-model typed fixtures below are NOT shipped adapters. They test public
// composition/exit/render policy without claiming complete production coverage.
type diagnosticQueries struct {
	t             *testing.T
	state         doctor.State
	calls         int
	tailscaleTrap bool
}

func (q *diagnosticQueries) datum(ctx context.Context) doctor.Datum {
	q.calls++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > doctor.GlobalBudget {
		q.t.Fatal("unbounded query")
	}
	evidence := doctor.EvidenceHealthy
	switch q.state {
	case doctor.Error:
		evidence = doctor.EvidenceFailed
	case doctor.Unknown:
		evidence = doctor.EvidenceUnavailable
	}
	return doctor.Datum{State: q.state, Evidence: evidence, Source: doctor.SourceQuery, At: time.Now()}
}
func (q *diagnosticQueries) Bundle(c context.Context, _ doctor.LocalTarget, _ doctor.BuildIdentity) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) Service(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) Relay(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) Herdr(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) Pairing(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) Cloudflare(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) Gateway(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	return q.datum(c)
}
func (q *diagnosticQueries) ManagedStatus(c context.Context, _ doctor.LocalTarget) doctor.Datum {
	if q.tailscaleTrap {
		q.t.Fatal("BYO invoked Tailscale")
	}
	return q.datum(c)
}
func (q *diagnosticQueries) CLIStatus(context.Context, doctor.LocalTarget) doctor.Datum {
	q.t.Fatal("disabled CLI invoked")
	return doctor.Datum{}
}

func TestDiagnosticStrictInvocationBeforeAcquisitionAndPublicErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "--json"}, {"--probe-ingress", "--probe-ingress"}, {"--config"}, {"--config", ""},
		{"--config", "private-config-canary"}, {"--config", "/private/../secret-command-canary"}, {"--config", "/private/\x1bsecret-command-canary"},
		{"--config", "/private/one", "--config", "/private/two"}, {"--json=true"}, {"--json", "--unknown=secret-command-canary"},
		{"--json", "https://private"}, {"--help"}, {"--", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out bytes.Buffer
			runtime := diagnosticRuntime{executable: func() (string, error) { t.Fatal("invalid invocation acquired package"); return "", nil }}
			code, err := runDoctorWithRuntime(context.Background(), args, false, &out, runtime)
			if code != 2 || err == nil || err.Error() != "invalid diagnostic invocation" || out.Len() != 0 || errors.Unwrap(err) != nil {
				t.Fatal("invocation error policy changed")
			}
			for _, command := range []string{"doctor", "status"} {
				actual := append([]string{command}, args...)
				code, err, stdout, stderr := captureDiagnosticDispatch(t, actual)
				if code != 2 || err == nil || stdout != "" || stderr != "herdr-mobile-relay: invalid diagnostic invocation\n" {
					t.Fatal("actual dispatch error output mismatch")
				}
				diagnosticPrivacy(t, stdout+stderr+err.Error())
			}
		})
	}
}

func captureDiagnosticDispatch(t *testing.T, args []string) (int, error, string, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	old := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = old }()
	code, result := runWithServerRunner(args, func(*app.Server, context.Context) error { t.Fatal("diagnostic entered app lifecycle"); return nil })
	if result != nil {
		reportErrorWithJournal(stderr, args, result, false)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	errors, err := io.ReadAll(stderr)
	if err != nil {
		t.Fatal(err)
	}
	return code, result, string(output), string(errors)
}

func TestDiagnosticActualPublicDispatchDoesNotLoadStartupConfiguration(t *testing.T) {
	runtime, selected, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=tailscale-cli\n")
	_ = runtime
	// Ordinary startup would refuse this CLI selection and malformed ambient
	// rearm/gateway policy. Diagnostic only reads the explicitly selected file.
	t.Setenv("HERDR_GATEWAY_URL", "wss://private-node-canary/invalid")
	t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "secret-command-canary")
	t.Setenv("HERDR_WEB_ROOT", "/private/config-canary")
	for _, command := range []string{"doctor", "status"} {
		code, err, out, stderr := captureDiagnosticDispatch(t, []string{command, "--json", "--config", selected})
		if err != nil || code != 3 || stderr != "" {
			t.Fatal("public diagnostic took startup/config error path")
		}
		doc := diagnosticDocument(t, out)
		if doc["exit_code"] != float64(code) {
			t.Fatal("public exit differs from JSON")
		}
		diagnosticPrivacy(t, out+stderr)
	}
}

func TestDiagnosticSelectorsAreDeterministicData(t *testing.T) {
	runtime, selected, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
	root := filepath.Dir(selected)
	for _, test := range []struct{ flag, env, dir, want string }{
		{selected, "/private/ignored", "/private/ignored-dir", selected},
		{"", selected, "/private/ignored-dir", selected},
		{"", "", root, filepath.Join(root, "relay.env")},
		{"", "", "", filepath.Join(root, "relay", ".env")},
		{"", "", "relative-private-canary", ""},
	} {
		seen := ""
		r := runtime
		r.getenv = func(key string) string {
			switch key {
			case "HERDR_RELAY_ENV":
				return test.env
			case "HERDR_PLUGIN_CONFIG_DIR":
				return test.dir
			default:
				t.Fatal("unexpected ambient read")
				return ""
			}
		}
		r.readConfig = func(path string) doctor.Inspection { seen = path; return doctor.Inspection{} }
		args := []string{"--json"}
		if test.flag != "" {
			args = append(args, "--config", test.flag)
		}
		var out bytes.Buffer
		code, err := runDoctorWithRuntime(context.Background(), args, false, &out, r)
		wantCode := 3
		if test.want == "" {
			wantCode = 2
		}
		if err != nil || code != wantCode || seen != test.want {
			t.Fatal("config selector precedence/normalization changed")
		}
		diagnosticDocument(t, out.String())
	}
}

func TestDiagnosticInvalidUnavailableConfigAndNonmutation(t *testing.T) {
	for _, kind := range []string{"literal valid", "missing", "world readable", "symlink", "executable syntax", "unknown mode", "duplicate", "private URL"} {
		t.Run(kind, func(t *testing.T) {
			runtime, selected, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
			root := filepath.Dir(selected)
			want := 3
			switch kind {
			case "missing":
				if err := os.Remove(selected); err != nil {
					t.Fatal(err)
				}
			case "world readable":
				if err := os.Chmod(selected, 0o644); err != nil {
					t.Fatal(err)
				}
				want = 2
			case "symlink":
				if err := os.Rename(selected, selected+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(selected+"-original", selected); err != nil {
					t.Fatal(err)
				}
				want = 2
			case "executable syntax":
				if err := os.WriteFile(selected, []byte("K=$(touch secret-command-canary)\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = 2
			case "unknown mode":
				if err := os.WriteFile(selected, []byte("HERDR_RELAY_TRANSPORT=private-node-canary\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = 2
			case "duplicate":
				if err := os.WriteFile(selected, []byte("K=one\nK=two\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = 2
			case "private URL":
				if err := os.WriteFile(selected, []byte("HERDR_GATEWAY_URL='wss://secret-command-canary@private-node-canary'\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = 2
			}
			before := diagnosticInventory(t, root)
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(context.Background(), []string{"--json"}, false, &out, runtime)
			if err != nil || code != want || diagnosticDocument(t, out.String())["exit_code"] != float64(want) {
				t.Fatal("valid JSON configuration refusal did not produce report/exit")
			}
			if diagnosticInventory(t, root) != before {
				t.Fatal("diagnostic created/migrated/chmodded/rewrote state")
			}
			diagnosticPrivacy(t, out.String())
		})
	}
}
func TestDarwinSelectedConfigAndRequiredBundleUnknown(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		return
	}
	runtime, selected, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
	runtime.readConfig, runtime.readIdentity = doctor.ReadConfig, doctor.ReadPackageIdentity
	before := diagnosticInventory(t, filepath.Dir(selected))
	var out bytes.Buffer
	code, err := runDoctorWithRuntime(context.Background(), []string{"--json", "--config", selected}, false, &out, runtime)
	if err != nil || code != 3 {
		t.Fatal("Darwin selected config became healthy, invalid or invocation-invalid")
	}
	document := diagnosticDocument(t, out.String())
	config, build := diagnosticWireCheck(t, document, doctor.CheckConfig), diagnosticWireCheck(t, document, doctor.CheckBuild)
	// A plain private 0600 config in private directories is admitted by
	// descriptor evidence; plain manifest metadata also proves ACL/volume safety.
	// This inventory is intentionally incomplete and cannot qualify the bundle.
	if document["exit_code"] != float64(3) || document["selected_transport"] != "cloudflare" ||
		config["state"] != "warn" || config["required"] != true || config["evidence_code"] != string(doctor.EvidenceTokenMissing) ||
		build["state"] != "ok" || build["required"] != true || build["evidence_code"] != string(doctor.EvidenceHealthy) {
		t.Fatal("Darwin config or proved package metadata was not admitted exactly")
	}
	bundle := diagnosticWireCheck(t, document, doctor.CheckBundle)
	if bundle["state"] != "unknown" || bundle["required"] != true || bundle["evidence_code"] != string(doctor.EvidenceUnavailable) {
		t.Fatal("Darwin selected config weakened required Bundle unavailability")
	}
	if before != diagnosticInventory(t, filepath.Dir(selected)) {
		t.Fatal("Darwin diagnostic mutated package or config")
	}
	diagnosticPrivacy(t, out.String())
}

func diagnosticInventory(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "%s %v %d %d\n", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, "%x\n", sha256.Sum256(data))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintln(&out, link)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestDiagnosticTypedFixtureExitAgreementAndPartialProduction(t *testing.T) {
	for _, state := range []doctor.State{doctor.OK, doctor.Error, doctor.Unknown} {
		runtime, _, build := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
		q := &diagnosticQueries{t: t, state: state}
		runtime.queries = func(string, doctor.Inspection, doctor.BuildIdentity) doctor.Dependencies {
			return doctor.Dependencies{Host: q, Transport: q}
		}
		want := 0
		if state == doctor.Error {
			want = 1
		}
		if state == doctor.Unknown {
			want = 3
		}
		for _, concise := range []bool{false, true} {
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(context.Background(), []string{"--json"}, concise, &out, runtime)
			if err != nil || code != want || diagnosticDocument(t, out.String())["exit_code"] != float64(want) {
				t.Fatal("typed fixture exit/JSON disagreement")
			}
			diagnosticPrivacy(t, out.String())
		}
		cfg := doctor.ParseConfig(strings.NewReader("HERDR_RELAY_TRANSPORT=cloudflare\n"))
		report := doctor.Run(context.Background(), cfg, build, doctor.Options{}, doctor.Dependencies{Host: q, Transport: q})
		var human, status, data bytes.Buffer
		if renderDiagnostic(report, &human, false, false) != nil || renderDiagnostic(report, &status, false, true) != nil || renderDiagnostic(report, &data, true, false) != nil {
			t.Fatal("render failed")
		}
		if !strings.Contains(human.String(), fmt.Sprintf("exit %d", want)) || !strings.Contains(status.String(), fmt.Sprintf("exit=%d", want)) || diagnosticDocument(t, data.String())["exit_code"] != float64(want) {
			t.Fatal("same finalized observation renderings disagree")
		}
		for _, check := range report.Checks() {
			if !strings.Contains(human.String(), string(check.ID)+": "+string(check.State)) || !strings.Contains(status.String(), string(check.ID)+"="+string(check.State)) {
				t.Fatal("rendered check decisions diverged")
			}
		}
	}
	// Actual shipped adapter remains incomplete even with valid metadata.
	runtime, selected, build := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
	cfg := doctor.ReadConfig(selected)
	r := doctor.Run(context.Background(), cfg, build, doctor.Options{}, runtime.queries(filepath.Dir(selected), cfg, build))
	if r.ExitCode() != 3 || diagnosticCheck(t, r, doctor.CheckBundle).State != doctor.Unknown {
		t.Fatal("partial adapter claimed full installed integrity")
	}
}

func TestDiagnosticMetadataFailureCancellationAndOutputErrors(t *testing.T) {
	for _, kind := range []string{"missing", "mismatch", "malformed", "executable error", "cancelled", "expired", "cancel during config", "cancel during metadata"} {
		t.Run(kind, func(t *testing.T) {
			runtime, selected, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\nHERDR_RELAY_INSTANCE_ID=private-instance-canary\n")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			root := filepath.Dir(selected)
			switch kind {
			case "missing":
				if err := os.Remove(filepath.Join(root, "release-manifest.json")); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				runtime.version = "0.99.9"
			case "malformed":
				if err := os.Chmod(filepath.Join(root, "release-manifest.json"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "release-manifest.json"), []byte("secret-command-canary"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "executable error":
				runtime.executable = func() (string, error) { return "", errors.New("private-error-canary") }
			case "cancelled":
				cancel()
			case "expired":
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
			case "cancel during config":
				runtime.readConfig = func(p string) doctor.Inspection { cancel(); return doctor.ReadConfig(p) }
			case "cancel during metadata":
				original := runtime.readIdentity
				runtime.readIdentity = func(p, v, r, target string) doctor.BuildIdentity { b := original(p, v, r, target); cancel(); return b }
			}
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(ctx, []string{"--json", "--config", selected}, false, &out, runtime)
			if err != nil || code != 3 {
				t.Fatal("unavailable acquisition invented health/failure")
			}
			doc := diagnosticDocument(t, out.String())
			if doc["relay_build"].(map[string]any)["version"] != "unknown" {
				t.Fatal("missing/late metadata accepted")
			}
			diagnosticPrivacy(t, out.String())
		})
	}
	runtime, _, _ := diagnosticFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
	for _, writer := range []io.Writer{diagnosticBadWriter{}, diagnosticShortWriter{}, nil} {
		code, err := runDoctorWithRuntime(context.Background(), []string{"--json"}, false, writer, runtime)
		if code != 1 || err == nil || err.Error() != "diagnostic output unavailable" || errors.Unwrap(err) != nil {
			t.Fatal("writer error leaked or silently succeeded")
		}
		diagnosticPrivacy(t, err.Error())
	}
}

type diagnosticBadWriter struct{}

func (diagnosticBadWriter) Write([]byte) (int, error) { return 0, errors.New("secret-output-canary") }

type diagnosticShortWriter struct{}

func (diagnosticShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestDiagnosticModeIngressAndBYOTraps(t *testing.T) {
	for _, mode := range []string{"cloudflare", "gateway", "tailscale-external", "tailscale", "tailscale-cli"} {
		text := "HERDR_RELAY_TRANSPORT=" + mode + "\n"
		if mode == "gateway" {
			text += "HERDR_GATEWAY_URL=wss://private-gateway.example.test\n"
		}
		if mode == "tailscale-external" {
			text += "HERDR_EXTERNAL_HTTPS_ORIGIN=https://private.example.test\n"
		}
		runtime, _, _ := diagnosticFixture(t, text)
		q := &diagnosticQueries{t: t, state: doctor.OK, tailscaleTrap: mode == "tailscale-external"}
		runtime.queries = func(string, doctor.Inspection, doctor.BuildIdentity) doctor.Dependencies {
			return doctor.Dependencies{Host: q, Transport: q}
		}
		var out bytes.Buffer
		code, err := runDoctorWithRuntime(context.Background(), []string{"--json", "--probe-ingress"}, false, &out, runtime)
		if err != nil || code != 3 {
			t.Fatal("missing requested ingress/disabled CLI qualified host")
		}
		diagnosticDocument(t, out.String())
		// With the actual factory, managed/CLI lack bound workflow identity and
		// all ordinary modes lack full categories: never complete host health.
		runtime.queries = productionDiagnosticRuntime().queries
		out.Reset()
		code, err = runDoctorWithRuntime(context.Background(), []string{"--json", "--probe-ingress"}, false, &out, runtime)
		if err != nil || code != 3 {
			t.Fatal("partial/unsupported actual mode observed complete health")
		}
		diagnosticPrivacy(t, out.String())
	}
}

// Hosted-only when executed by later authorization: real production transport,
// disposable loopback server, actual metadata/config readers and command path.
func TestDiagnosticActualLocalCompositionStillIncomplete(t *testing.T) {
	for _, mode := range []string{"cloudflare", "gateway", "tailscale-external"} {
		t.Run(mode, func(t *testing.T) {
			text := "HERDR_RELAY_TRANSPORT=" + mode + "\nHERDR_RELAY_INSTANCE_ID=private-instance-canary\n"
			if mode == "gateway" {
				text += "HERDR_GATEWAY_URL=wss://private.example.test\n"
			}
			if mode == "tailscale-external" {
				text += "HERDR_EXTERNAL_HTTPS_ORIGIN=https://private.example.test\n"
			}
			runtime, selected, build := diagnosticBundleFixture(t, text)
			// Actual complete package verification plus genuine local queries
			// through installed shim/current, not just injected successful data.
			executable, err := runtime.executable()
			if err != nil {
				t.Fatal(err)
			}
			shim, _, _ := diagnosticInstalledShim(t, filepath.Dir(executable))
			runtime.executable = func() (string, error) { return shim, nil }
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("non-diagnostic request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("X-Herdr-Relay-Instance", "private-instance-canary")
				if r.URL.Path == "/health" {
					w.Header().Set("Content-Type", "text/plain")
					fmt.Fprint(w, "ok\n")
					return
				}
				if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
					t.Error("external/arbitrary content query")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				object := map[string]any{"instance": "private-instance-canary", "version": build.Version, "release_version": build.Version,
					"revision": build.Revision, "protocol": protocol.Version, "bundle_hash": build.WebHash, "bundle_version": build.Version,
					"bundle_revision": build.Revision, "bundle_build": strings.Repeat("d", 64), "status": "ready",
					"inventory": map[string]any{"state": "ready", "stale": false, "error_code": "", "last_attempt_at": time.Now().Unix(), "last_success_at": time.Now().Unix()}}
				if r.URL.Path == "/healthz" {
					object["status"], object["readiness"], object["transport"] = "ok", "ready", mode
					object["gateway"] = map[string]any{"enabled": true, "registered": true, "relay_id": "AAAAAAAAAAAAAAAAAAAAAA", "url": "wss://private.example.test", "urls": []string{"wss://private.example.test"}}
				}
				if err := json.NewEncoder(w).Encode(object); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := strconv.Atoi(port); err != nil {
				t.Fatal(err)
			}
			text += "HERDR_RELAY_PORT=" + port + "\n"
			if err := os.WriteFile(selected, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			before := diagnosticInventory(t, filepath.Dir(selected))
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(context.Background(), []string{"--json", "--probe-ingress"}, false, &out, runtime)
			if err != nil || code != 3 {
				t.Fatal("local registration/health became whole-host success")
			}
			doc := diagnosticDocument(t, out.String())
			for _, item := range doc["checks"].([]any) {
				check := item.(map[string]any)
				id := check["id"]
				if id == string(doctor.CheckRelay) || (mode == "gateway" && id == string(doctor.CheckGateway)) {
					if check["state"] != "ok" {
						t.Fatal("actual exact local composition not observed")
					}
				}
				if id == string(doctor.CheckBundle) && (check["state"] != diagnosticExpectedBundleState("ok") || check["required"] != true) {
					t.Fatal("actual package bytes or unestablished ACL safety policy diverged")
				}
				if id == string(doctor.CheckIngress) && (check["state"] != "unknown" || check["required"] != true) {
					t.Fatal("required ingress observation manufactured")
				}
			}
			want := 3
			if mode == "gateway" {
				want = 6
			}
			if requests.Load() != int32(want) || diagnosticInventory(t, filepath.Dir(selected)) != before {
				t.Fatal("unexpected query/mutation count")
			}
			diagnosticPrivacy(t, out.String())
		})
	}
}

// Independent builder-shaped command fixture. Config lives OUTSIDE the public
// package: a manifest cannot authorize opening private config as bundle content.
// Binary/wrappers/compressed files are inert and are never executed. release.Build
// authors fixture metadata only; the command uses the actual diagnostic verifier.
func diagnosticBundleFixture(t *testing.T, text string) (diagnosticRuntime, string, doctor.BuildIdentity) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "package")
	write := func(name string, data []byte, mode os.FileMode) {
		filename := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	wrappers := []string{"common.sh", "native-install-transaction.sh", "herdr-mobile-relay-service.sh", "install-service.sh", "install-systemd-user-service.sh", "plugin-on-event.sh", "plugin-status.sh", "pi-commands.sh", "service.sh", "setup-link.sh", "setup.sh", "speech-voices.sh", "stable-setup.sh", "stable-teardown.sh", "start.sh", "tailscale.sh", "tailscale-external.sh", "tailscale-cli.sh", "tailscale-cli-service.sh", "transport-switch-locked.sh", "uninstall.sh", "uninstall-service.sh", "uninstall-systemd-user-service.sh"}
	for _, name := range wrappers {
		write("relay/"+name, []byte("inert wrapper bytes, never executed\n"), 0o755)
	}
	for _, name := range []string{"LICENSE", "README.md", ".env.example", "relay/pi-command-bridge/bridge.mjs", "relay/pi-command-bridge/index.ts"} {
		write(name, []byte("synthetic public fixture\n"), 0o644)
	}
	write("herdr-mobile-relay", []byte("inert executable shape, never executed\n"), 0o755)
	for _, name := range []string{"_headers", "_redirects", "index.html", "herdr-bootstrap.js", "manifest-loader.js", "manifest.webmanifest", "notification-icons.js", "release.json", "setup.webmanifest", "sw.js", "version.json", "fonts/nerd-symbols-mono-v3.4.0.license.txt", "fonts/nerd-symbols-mono-v3.4.0.woff2", "icons/apple-touch-icon.png", "icons/icon-192.png", "icons/icon-512.png", "icons/icon-maskable-512.png", "icons/icon.svg", "icons/notification-badge.png"} {
		write("web/"+name, []byte("synthetic public web fixture\n"), 0o644)
	}
	digest := func(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
	sri := func(hash string) string {
		raw, err := hex.DecodeString(hash)
		if err != nil {
			t.Fatal(err)
		}
		return "sha256-" + base64.StdEncoding.EncodeToString(raw)
	}
	js, css := []byte("/* synthetic script */"), []byte("/* synthetic style */")
	jsHash, cssHash := digest(js), digest(css)
	jsPath, cssPath := "assets/app-"+jsHash+".js", "assets/app-"+cssHash+".css"
	entryPath := "builds/0.22.4-1-" + strings.Repeat("a", 16) + "/index.html"
	entry := []byte(`<link href="/` + cssPath + `" integrity="` + sri(cssHash) + `"><script src="/` + jsPath + `" integrity="` + sri(jsHash) + `"></script>`)
	descriptor := release.WebDescriptor{Schema: 1, Version: "0.22.4", Assets: 1, Build: strings.Repeat("a", 64), Entry: "/" + entryPath,
		Files: map[string]release.WebDescriptorFile{
			"entry":      {Path: entryPath, SHA256: digest(entry), Integrity: sri(digest(entry))},
			"javascript": {Path: jsPath, SHA256: jsHash, Integrity: sri(jsHash)},
			"stylesheet": {Path: cssPath, SHA256: cssHash, Integrity: sri(cssHash)},
		}}
	for name, data := range map[string][]byte{entryPath: entry, jsPath: js, cssPath: css, "assets/ConversationHistory-1.js": []byte("synthetic lazy content")} {
		write("web/"+name, data, 0o644)
		write("web/"+name+".br", []byte("synthetic compressed content"), 0o644)
	}
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	write("web/release.json", data, 0o644)
	manifest, err := release.Build(root, "0.22.4", strings.Repeat("b", 40), release.CurrentTarget())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := release.Verify(root, release.CurrentTarget()); err != nil {
		t.Fatal("synthetic package does not meet actual release contract")
	}
	build := doctor.BuildIdentity{Version: manifest.Version, Revision: manifest.Revision, WebHash: manifest.WebHash, Target: manifest.Target}
	selected := filepath.Join(base, "private-config-canary")
	if err := os.WriteFile(selected, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := productionDiagnosticRuntime()
	runtime.executable = func() (string, error) { return filepath.Join(root, "herdr-mobile-relay"), nil }
	runtime.version, runtime.revision, runtime.target = build.Version, build.Revision, build.Target
	runtime.getenv = func(key string) string {
		switch key {
		case "HERDR_RELAY_ENV":
			return selected
		case "HERDR_PLUGIN_CONFIG_DIR":
			return ""
		default:
			t.Fatal("diagnostic read ambient package/credential authority")
			return ""
		}
	}
	metadataBytes, err := os.ReadFile(filepath.Join(root, "release-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	installDarwinSupportedHostFixtures(&runtime, root, metadataBytes, build)
	return runtime, selected, build
}

// Plain private fixtures can prove Linux or Darwin bundle evidence; an advertised
// manifest target cannot establish evidence on an unsupported host.
func diagnosticExpectedBundleState(state string) string {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		return "unknown"
	}
	return state
}
func diagnosticWireCheck(t *testing.T, document map[string]any, id doctor.CheckID) map[string]any {
	t.Helper()
	for _, value := range document["checks"].([]any) {
		check := value.(map[string]any)
		if check["id"] == string(id) {
			return check
		}
	}
	t.Fatal("missing required wire check")
	return nil
}
func TestDiagnosticActualBundleCompositionRemainsPartialAndPrivate(t *testing.T) {
	for _, mode := range []string{"cloudflare", "gateway", "tailscale-external", "tailscale", "tailscale-cli"} {
		t.Run(mode, func(t *testing.T) {
			text := "HERDR_RELAY_TRANSPORT=" + mode + "\n"
			if mode == "gateway" {
				text += "HERDR_GATEWAY_URL=wss://private-node-canary.invalid\n"
			}
			if mode == "tailscale-external" {
				text += "HERDR_EXTERNAL_HTTPS_ORIGIN=https://private-node-canary.invalid\n"
			}
			runtime, selected, _ := diagnosticBundleFixture(t, text)
			before := diagnosticInventory(t, filepath.Dir(selected))
			// These cannot select a package root/hash or permission to call CLI.
			t.Setenv("HERDR_WEB_ROOT", "/private/path-canary")
			t.Setenv("HERDR_RELEASE_ROOT", "/private/path-canary")
			t.Setenv("HERDR_RELAY_BIN", "/private/path-canary")
			t.Setenv("HERDR_RELAY_TOKEN", "secret-command-canary")
			t.Setenv("HERDR_RELAY_REARM_BOOTSTRAP", "secret-command-canary")
			for _, concise := range []bool{false, true} {
				var out bytes.Buffer
				code, err := runDoctorWithRuntime(context.Background(), []string{"--json", "--probe-ingress"}, concise, &out, runtime)
				if err != nil || code != 3 {
					t.Fatal("bundle consistency qualified incomplete host/ingress")
				}
				doc := diagnosticDocument(t, out.String())
				bundle := diagnosticWireCheck(t, doc, doctor.CheckBundle)
				if bundle["state"] != diagnosticExpectedBundleState("ok") {
					t.Fatal("actual byte verifier or unestablished ACL safety policy not composed")
				}
				if bundle["required"] != true || (diagnosticExpectedBundleState("ok") == "unknown" && bundle["evidence_code"] != string(doctor.EvidenceUnavailable)) {
					t.Fatal("unestablished ACL safety did not retain required unavailable evidence")
				}
				for _, id := range []doctor.CheckID{doctor.CheckService, doctor.CheckRelay, doctor.CheckHerdr, doctor.CheckPairing, doctor.CheckIngress, doctor.CheckIngressBundle} {
					if diagnosticWireCheck(t, doc, id)["state"] != "unknown" {
						t.Fatal("bundle success changed other categories")
					}
				}
				if mode == "tailscale-cli" && diagnosticWireCheck(t, doc, doctor.CheckCLI)["evidence_code"] != string(doctor.EvidenceDisabled) {
					t.Fatal("selection enabled CLI inspection")
				}
				diagnosticPrivacy(t, out.String())
			}
			for _, concise := range []bool{false, true} {
				var out bytes.Buffer
				code, err := runDoctorWithRuntime(context.Background(), nil, concise, &out, runtime)
				marker := string(doctor.CheckBundle) + ": " + diagnosticExpectedBundleState("ok")
				if concise {
					marker = string(doctor.CheckBundle) + "=" + diagnosticExpectedBundleState("ok")
				}
				if err != nil || code != 3 || !strings.Contains(out.String(), marker) {
					t.Fatal("human/status byte decision diverged")
				}
				diagnosticPrivacy(t, out.String())
			}
			if before != diagnosticInventory(t, filepath.Dir(selected)) {
				t.Fatal("public diagnostic mutated package/config")
			}
		})
	}
}

// F001: emulate execute permission disappearing after OS executable discovery,
// while the diagnostic process is already running. The binary is never run.
// Unproved host evidence overrides stable failure; the plain Darwin fixture can prove ACL safety.
func TestDiagnosticNonexecutablePackageAfterDiscoveryIsObservedFailure(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			text := "HERDR_RELAY_TRANSPORT=cloudflare\n"
			if invalid {
				text = "K=$(touch secret-command-canary)\n"
			}
			runtime, selected, build := diagnosticBundleFixture(t, text)
			executable, err := runtime.executable()
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(executable)
			original := runtime.readIdentity
			reads := 0
			expectedInventory := ""
			runtime.readIdentity = func(physical, version, revision, target string) doctor.BuildIdentity {
				if physical != root {
					t.Fatal("shape regression changed executing-package authority")
				}
				observed := original(physical, version, revision, target)
				if observed != build {
					t.Fatal("shape regression lost compiled-bound metadata")
				}
				// Only this disposable fixture seam mutates modes; acquisition then
				// continues through the actual production adapter and real runner.
				if err := os.Chmod(executable, 0o644); err != nil {
					t.Fatal(err)
				}
				reads++
				expectedInventory = diagnosticInventory(t, filepath.Dir(selected))
				return observed
			}
			for _, jsonMode := range []bool{false, true} {
				for _, concise := range []bool{false, true} {
					if err := os.Chmod(executable, 0o755); err != nil {
						t.Fatal(err)
					}
					args := []string(nil)
					if jsonMode {
						args = []string{"--json"}
					}
					var out bytes.Buffer
					code, err := runDoctorWithRuntime(context.Background(), args, concise, &out, runtime)
					wantCode, wantState := 1, diagnosticExpectedBundleState("error")
					if wantState == "unknown" {
						wantCode = 3
					}
					if invalid {
						wantCode, wantState = 2, "unknown"
					}
					if err != nil || code != wantCode {
						t.Fatal("stable package shape failure/invalid-config priority diverged")
					}
					if jsonMode {
						doc := diagnosticDocument(t, out.String())
						check := diagnosticWireCheck(t, doc, doctor.CheckBundle)
						if check["state"] != wantState || doc["exit_code"] != float64(wantCode) {
							t.Fatal("shape failure JSON/decision mismatch")
						}
						if wantCode == 1 && (check["evidence_code"] != string(doctor.EvidenceFailed) || check["required"] != true) {
							t.Fatal("stable shape contradiction lacks observed required failure")
						}
						if diagnosticWireCheck(t, doc, doctor.CheckBuild)["state"] != "ok" {
							t.Fatal("shape failure conflated with metadata availability")
						}
					} else {
						marker := string(doctor.CheckBundle) + ": " + wantState
						if concise {
							marker = string(doctor.CheckBundle) + "=" + wantState
						}
						if !strings.Contains(out.String(), marker) {
							t.Fatal("shape failure human/status diverged")
						}
					}
					if expectedInventory == "" || expectedInventory != diagnosticInventory(t, filepath.Dir(selected)) {
						t.Fatal("diagnostic changed the post-discovery snapshot")
					}
					diagnosticPrivacy(t, out.String())
				}
			}
			if reads != 4 {
				t.Fatal("post-discovery shape regression bypassed metadata acquisition")
			}
		})
	}
}

type diagnosticCancelBundle struct {
	doctor.HostQueries
	cancel context.CancelFunc
}

func (q diagnosticCancelBundle) Bundle(ctx context.Context, target doctor.LocalTarget, build doctor.BuildIdentity) doctor.Datum {
	observed := q.HostQueries.Bundle(ctx, target, build)
	q.cancel()
	return observed
}
func TestDiagnosticBundleFailureUnknownPriorityAndLateResult(t *testing.T) {
	for _, kind := range []string{"hash", "missing", "unsafe", "metadata missing", "invalid config", "cancel after actual bundle"} {
		t.Run(kind, func(t *testing.T) {
			runtime, selected, _ := diagnosticBundleFixture(t, "HERDR_RELAY_TRANSPORT=cloudflare\n")
			executable, err := runtime.executable()
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(executable)
			wantCode, wantState := 3, "unknown"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "hash", "invalid config":
				if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("changed bytes"), 0o644); err != nil {
					t.Fatal(err)
				}
				wantCode, wantState = 1, diagnosticExpectedBundleState("error")
				if wantState == "unknown" {
					wantCode = 3
				}
				if kind == "invalid config" {
					if err := os.WriteFile(selected, []byte("K=$(touch secret-command-canary)\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					wantCode, wantState = 2, "unknown"
				}
			case "missing":
				if err := os.Remove(filepath.Join(root, "LICENSE")); err != nil {
					t.Fatal(err)
				}
			case "unsafe":
				if err := os.Chmod(filepath.Join(root, "LICENSE"), 0o666); err != nil {
					t.Fatal(err)
				}
			case "metadata missing":
				if err := os.Remove(filepath.Join(root, "release-manifest.json")); err != nil {
					t.Fatal(err)
				}
			case "cancel after actual bundle":
				original := runtime.queries
				runtime.queries = func(root string, cfg doctor.Inspection, build doctor.BuildIdentity) doctor.Dependencies {
					deps := original(root, cfg, build)
					deps.Host = diagnosticCancelBundle{deps.Host, cancel}
					return deps
				}
			}
			before := diagnosticInventory(t, filepath.Dir(selected))
			var out bytes.Buffer
			code, err := runDoctorWithRuntime(ctx, []string{"--json"}, false, &out, runtime)
			if err != nil || code != wantCode {
				t.Fatal("bundle finite decision/priority changed")
			}
			doc := diagnosticDocument(t, out.String())
			if diagnosticWireCheck(t, doc, doctor.CheckBundle)["state"] != wantState || doc["exit_code"] != float64(wantCode) {
				t.Fatal("bundle report/exit mismatch")
			}
			if kind != "metadata missing" && diagnosticWireCheck(t, doc, doctor.CheckBuild)["state"] != "ok" {
				t.Fatal("metadata availability conflated with actual byte result")
			}
			if before != diagnosticInventory(t, filepath.Dir(selected)) {
				t.Fatal("failure/cancellation changed state")
			}
			diagnosticPrivacy(t, out.String())
		})
	}
}
