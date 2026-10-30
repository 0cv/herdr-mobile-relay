package tailscaleserve

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fakeCLI = `#!/bin/sh
printf '%s|%s\n' "$TAILSCALE_BE_CLI" "$*" >> "$TS_LOG"
case "$*" in
  version)
    if [ "$TS_FAIL" = version ]; then echo 'version unavailable' >&2; exit 1; fi
    printf '%s\n' "$TS_VERSION" ;;
  'status --json')
    if [ "$TS_FAIL" = status ]; then echo 'not logged in' >&2; exit 1; fi
    if [ "$TS_FAIL" = permission_status ]; then echo 'Access denied' >&2; exit 1; fi
    printf '%s\n' "$TS_STATUS" ;;
  'serve status --json')
    if [ "$TS_FAIL" = permission ]; then echo 'Access denied: permission denied' >&2; exit 1; fi
    if [ -n "$TS_APPLIED" ] && [ -f "$TS_APPLIED" ]; then
      printf '%s\n' "$TS_APPLIED_STATUS"
    else
      printf '%s\n' "$TS_SERVE_STATUS"
    fi ;;
  serve*off)
    if [ "$TS_FAIL" = https ]; then echo 'error enabling https feature' >&2; exit 1; fi
    if [ -n "$TS_APPLIED" ]; then /bin/rm -f "$TS_APPLIED"; fi ;;
  serve*)
    if [ "$TS_FAIL" = applied ]; then : > "$TS_APPLIED"; echo 'serve failed after applying mapping' >&2; exit 1; fi
    if [ "$TS_FAIL" = https ]; then echo 'error enabling https feature' >&2; exit 1; fi
    if [ "$TS_FAIL" = permission ]; then echo 'Access denied' >&2; exit 1; fi
    if [ "$TS_FAIL" = foreground ]; then echo 'HTTPS cert support is not enabled/configured for your tailnet.' >&2; exit 1; fi
    if [ "$TS_FOREGROUND" = 1 ]; then
      trap 'echo signal >> "$TS_LOG"; exit 0' INT TERM
      : > "$TS_READY"
      while :; do /bin/sleep 0.1; done
    fi ;;
esac
`

func setup(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	originalBundle := systemBundle
	systemBundle = filepath.Join(root, "system/Tailscale.app/Contents/MacOS/Tailscale")
	t.Cleanup(func() { systemBundle = originalBundle })
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "tailscale")
	if err := os.WriteFile(binary, []byte(fakeCLI), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", root)
	t.Setenv("HERDR_TAILSCALE_BIN", "")
	t.Setenv("TS_LOG", filepath.Join(root, "log"))
	t.Setenv("TS_VERSION", "1.52.0")
	t.Setenv("TS_STATUS", `{"BackendState":"Running","Self":{"DNSName":"node.tail.ts.net."},"CertDomains":["node.tail.ts.net"]}`)
	t.Setenv("TS_SERVE_STATUS", `{}`)
	t.Setenv("TS_FAIL", "")
	return root, binary
}

func invoke(args ...string) (int, string, error) {
	var out, diagnostics bytes.Buffer
	code, err := Run(args, &out, &diagnostics)
	if err != nil {
		diagnostics.WriteString(err.Error())
	}
	return code, out.String() + diagnostics.String(), err
}

func logLines(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("TS_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestResolution(t *testing.T) {
	root, binary := setup(t)
	other := filepath.Join(root, "explicit")
	if err := os.WriteFile(other, []byte(fakeCLI), 0700); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(os.Getenv("HOME"), "Applications/Tailscale.app/Contents/MacOS/Tailscale")
	if err := os.MkdirAll(filepath.Dir(bundle), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundle, []byte("#!/bin/sh\necho bundle >> \"$TS_LOG\"\n"+strings.TrimPrefix(fakeCLI, "#!/bin/sh\n")), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TAILSCALE_BIN", other)
	if code, _, _ := invoke("check"); code != 0 {
		t.Fatal("explicit binary failed")
	}
	t.Setenv("HERDR_TAILSCALE_BIN", "")
	if code, _, _ := invoke("check"); code != 0 || strings.Contains(logLines(t), "bundle") {
		t.Fatal("PATH did not take precedence over bundle")
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := invoke("check"); code != 0 {
		t.Fatal("App Store bundle failed")
	}
	if got := logLines(t); strings.Count(got, "1|version\n") != 3 || !strings.Contains(got, "bundle\n") {
		t.Fatalf("TAILSCALE_BE_CLI missing: %s", got)
	}
	if err := os.MkdirAll(filepath.Dir(systemBundle), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(systemBundle, []byte(fakeCLI), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := invoke("check"); code != 0 {
		t.Fatal("system bundle failed")
	}
}

func TestFailures(t *testing.T) {
	root, binary := setup(t)
	cases := []struct {
		name, key, value, operation, want string
		code                              int
	}{
		{"override relative", "HERDR_TAILSCALE_BIN", "tailscale", "check", "absolute executable", Missing},
		{"override not executable", "HERDR_TAILSCALE_BIN", filepath.Join(root, "log"), "check", "absolute executable", Missing},
		{"old version", "TS_VERSION", "1.51.9", "check", "1.52 or newer", Old},
		{"version failure", "TS_FAIL", "version", "check", "version unavailable", 1},
		{"bad version", "TS_VERSION", "unknown", "check", "cannot read Tailscale version", 1},
		{"status failure", "TS_FAIL", "status", "check", "not logged in or running", Stopped},
		{"status permission", "TS_FAIL", "permission_status", "check", "sudo tailscale set --operator=$USER", Permission},
		{"stopped", "TS_STATUS", `{"BackendState":"Stopped"}`, "check", "BackendState", Stopped},
		{"no DNS", "TS_STATUS", `{"BackendState":"Running","Self":{"DNSName":""},"CertDomains":["x"]}`, "check", "MagicDNS", NoDNS},
		{"no certs", "TS_STATUS", `{"BackendState":"Running","Self":{"DNSName":"node.tail.ts.net."}}`, "check", "HTTPS certificates", NoHTTPS},
		{"bad JSON", "TS_STATUS", `nope`, "check", "invalid tailscale status", 1},
		{"https refusal", "TS_FAIL", "https", "serve", "HTTPS certificates", NoHTTPS},
		{"permission", "TS_FAIL", "permission", "serve", "sudo tailscale set --operator=$USER", Permission},
		{"bad serve JSON", "TS_SERVE_STATUS", `nope`, "serve", "invalid tailscale serve status", 1},
		{"foreground refusal", "TS_FAIL", "foreground", "serve", "HTTPS certificates", NoHTTPS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			args := []string{tc.operation}
			if tc.operation == "serve" && tc.name != "foreground refusal" {
				args = append(args, "--bg")
			}
			code, message, _ := invoke(args...)
			if code != tc.code || !strings.Contains(message, tc.want) {
				t.Fatalf("code %d message %q, want %d and %q", code, message, tc.code, tc.want)
			}
		})
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}
	if code, message, _ := invoke("check"); code != Missing || !strings.Contains(message, "not installed") {
		t.Fatalf("missing CLI: %d %s", code, message)
	}
}

func TestAddressAndUsage(t *testing.T) {
	setup(t)
	var out, diagnostics bytes.Buffer
	if code, err := Run([]string{"address", "--port", "443"}, &out, &diagnostics); code != 0 || err != nil || out.String() != "https://node.tail.ts.net\n" || diagnostics.Len() != 0 {
		t.Fatalf("443: %d %v %q %q", code, err, out.String(), diagnostics.String())
	}
	out.Reset()
	if code, err := Run([]string{"address", "--port", "9443"}, &out, &diagnostics); code != 0 || err != nil || out.String() != "https://node.tail.ts.net:9443\n" {
		t.Fatalf("custom port: %d %v %q", code, err, out.String())
	}
	for _, args := range [][]string{{}, {"wat"}, {"off", "--bg"}, {"serve", "--port", "0"}, {"serve", "--relay-port", "65536"}, {"check", "extra"}} {
		if code, _, _ := invoke(args...); code != Usage {
			t.Fatalf("%v accepted", args)
		}
	}
}

const own = `{"TCP":{"8443":{"HTTPS":true},"9443":{"HTTPS":true}},"Web":{"node.tail.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}},"node.tail.ts.net:9443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:1234"}}}}}`

func TestPublishAndOff(t *testing.T) {
	setup(t)
	if code, _, _ := invoke("serve", "--bg"); code != 0 {
		t.Fatal("publish failed")
	}
	if code, _, _ := invoke("serve", "--bg", "--port", "9443", "--relay-port", "9999"); code != 0 || !strings.Contains(logLines(t), "1|serve --bg --yes --https=9443 http://127.0.0.1:9999\n") {
		t.Fatal("custom ports failed")
	}
	if !strings.Contains(logLines(t), "1|serve --bg --yes --https=8443 http://127.0.0.1:8375\n") {
		t.Fatal(logLines(t))
	}
	t.Setenv("TS_SERVE_STATUS", own)
	if code, message, _ := invoke("serve"); code != Conflict || !strings.Contains(message, "already in use") {
		t.Fatalf("foreground serve reused a mapping without a child: %d %s", code, message)
	}
	before := logLines(t)
	if code, _, _ := invoke("serve", "--bg"); code != 0 || logLines(t) != before+"1|version\n1|status --json\n1|serve status --json\n" {
		t.Fatal("idempotent serve changed the mapping")
	}
	if code, _, _ := invoke("off"); code != 0 || !strings.HasSuffix(logLines(t), "1|serve --https=8443 off\n") {
		t.Fatal("off did not disable only 8443")
	}
	before = logLines(t)
	if code, _, _ := invoke("off", "--port", "9443"); code != 0 || strings.Contains(strings.TrimPrefix(logLines(t), before), " off\n") {
		t.Fatal("off removed unrelated mapping")
	}
	before = logLines(t)
	if code, _, _ := invoke("off", "--relay-port", "9999"); code != 0 || strings.Contains(strings.TrimPrefix(logLines(t), before), " off\n") {
		t.Fatal("off removed another target on the same port")
	}
	t.Setenv("TS_STATUS", `{"BackendState":"Running","Self":{"DNSName":"node.tail.ts.net."}}`)
	before = logLines(t)
	if code, _, _ := invoke("off"); code != 0 || !strings.HasSuffix(logLines(t), "1|serve --https=8443 off\n") {
		t.Fatal("off could not remove mapping after certificates were disabled")
	}
	t.Setenv("TS_SERVE_STATUS", `{}`)
	before = logLines(t)
	if code, _, _ := invoke("off"); code != 0 || strings.Contains(strings.TrimPrefix(logLines(t), before), " off\n") {
		t.Fatal("off removed absent mapping")
	}
}

func TestFailedPublishRemovesOnlyOwnedMapping(t *testing.T) {
	root, _ := setup(t)
	t.Setenv("TS_APPLIED", filepath.Join(root, "applied"))
	t.Setenv("TS_APPLIED_STATUS", own)
	t.Setenv("TS_SERVE_STATUS", `{"TCP":{"9443":{"HTTPS":true}},"Web":{"node.tail.ts.net:9443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:1234"}}}}}`)
	t.Setenv("TS_FAIL", "applied")
	if code, message, _ := invoke("serve", "--bg"); code != 1 || !strings.Contains(message, "serve failed after applying mapping") {
		t.Fatalf("failed publish: %d %s", code, message)
	}
	if _, err := os.Stat(filepath.Join(root, "applied")); !os.IsNotExist(err) {
		t.Fatalf("mapping was not cleared: %v; calls: %s", err, logLines(t))
	}
	if used, owned, err := (client{binary: filepath.Join(root, "tailscale"), env: append(os.Environ(), "TAILSCALE_BE_CLI=1")}).mapping(9443, "http://127.0.0.1:1234"); err != nil || !used || !owned {
		t.Fatalf("unrelated mapping was changed: %v", err)
	}
	if got := logLines(t); !strings.Contains(got, "1|serve status --json\n1|serve --https=8443 off\n") || strings.Contains(got, "--https=9443 off") {
		t.Fatalf("cleanup touched another port or missed ours: %s", got)
	}
}

func TestConflicts(t *testing.T) {
	setup(t)
	for _, config := range []string{
		`{"TCP":{"8443":{"HTTPS":true}},"Web":{"node.tail.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9000"}}}}}`,
		`{"TCP":{"8443":{"HTTPS":false}}}`,
		`{"TCP":{"8443":{"HTTPS":true}},"Web":{"node.tail.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"},"/other":{"Proxy":"http://127.0.0.1:9000"}}}}}`,
		own[:len(own)-1] + `,"AllowFunnel":{"node.tail.ts.net:8443":true}}`,
		`{"Foreground":{"session":{"TCP":{"8443":{"HTTPS":true}},"Web":{"node.tail.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9000"}}}}}}}`,
	} {
		t.Setenv("TS_SERVE_STATUS", config)
		if code, message, _ := invoke("serve", "--bg"); code != Conflict || !strings.Contains(message, "already in use") {
			t.Fatalf("%s: %d %s", config, code, message)
		}
		before := logLines(t)
		if code, _, _ := invoke("off"); code != 0 || strings.Contains(strings.TrimPrefix(logLines(t), before), " off\n") {
			t.Fatalf("off removed conflicting mapping: %s", config)
		}
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("TS_HELPER") != "1" {
		return
	}
	var out, diagnostics bytes.Buffer
	code, _ := Run([]string{"serve"}, &out, &diagnostics)
	os.Exit(code)
}

func TestForegroundForwardsSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			root, _ := setup(t)
			ready := filepath.Join(root, "ready")
			t.Setenv("TS_READY", ready)
			t.Setenv("TS_FOREGROUND", "1")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
			cmd.Env = append(os.Environ(), "TS_HELPER=1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("foreground CLI never started")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			for time.Now().Before(deadline) {
				data, _ := os.ReadFile(os.Getenv("TS_LOG"))
				if strings.Contains(string(data), "signal\n") {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("signal was not forwarded")
		})
	}
}
