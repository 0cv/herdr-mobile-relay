package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func parseFixture(text string) Inspection { return ParseConfig(strings.NewReader(text)) }
func fixtureConfig(mode Mode) Inspection {
	text := "HERDR_RELAY_TOKEN='" + strings.Repeat("k", 32) + "'\nHERDR_RELAY_TRANSPORT='" + string(mode) + "'\n"
	switch mode {
	case ModeGateway: text += "HERDR_GATEWAY_URL='wss://gateway.example.test'\nHERDR_PHONE_APP_URL='https://app.example.test'\n"
	case ModeBYO: text += "HERDR_EXTERNAL_HTTPS_ORIGIN='https://relay.example.test'\n"
	case ModeManaged: text += "HERDR_TAILSCALE_ORIGIN='https://relay.example.test'\n"
	case ModeCLI: text += "HERDR_TAILSCALE_CLI_ORIGIN='https://relay.example.test:8443'\n"
	}
	return parseFixture(text)
}

func TestLiteralConfigGeneratedQuotingAndLegacy(t *testing.T) {
	for _, value := range []string{"", "simple", "owner's selected app", "''", "a'b'c", "$literal;not a command", "/private/user's config"} {
		// Exact common.sh shell_quote_value emission, not a shell execution.
		raw := "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
		got, ok := literal(raw)
		if !ok || got != value { t.Fatal("generated literal quoting did not round trip") }
		if parseFixture("UNKNOWN_KEY="+raw+"\n").Code() != ConfigValid { t.Fatal("benign unknown quoted key was refused") }
	}
	for _, input := range []string{"HERDR_RELAY_PORT=8375\n", "export HERDR_RELAY_PORT=\"8375\"\n", "# comment\nHERDR_RELAY_PORT='8375'\n"} {
		cfg := parseFixture(input)
		if cfg.Code() != ConfigValid || cfg.Mode() != ModeCloudflare || cfg.port != 8375 { t.Fatal("simple legacy literal failed") }
	}
	c := parseFixture("GH_TOKEN='secret-canary'\nHERDR_GITHUB_TOKEN_FILE='/private/token-canary'\nUNKNOWN_KEY='node-canary'\n")
	if c.Code() != ConfigValid || c.tokenPresent { t.Fatal("ambient/updater keys were merged into relay authentication") }
	if _, err := json.Marshal(c); err == nil { t.Fatal("private configuration was serializable") }
}

func TestLiteralConfigExpectedInstanceIsPrivateAndSelected(t *testing.T) {
	for _, instance := range []string{"", strings.Repeat("a", 32), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "private:instance@fixture", strings.Repeat("x", 128), "owner's-instance"} {
		raw := "'" + strings.ReplaceAll(instance, "'", "'\\''") + "'"
		cfg := parseFixture("HERDR_RELAY_INSTANCE_ID="+raw+"\n")
		if cfg.Code() != ConfigValid || cfg.instance != instance { t.Fatal("bounded instance grammar/quoting diverged") }
		if cfg.String() != "private diagnostic configuration" || cfg.GoString() != cfg.String() { t.Fatal("private identity reflected") }
		if _, err := json.Marshal(cfg); err == nil || strings.Contains(err.Error(), "private:instance") { t.Fatal("identity serialized/reflected") }
	}
	for _, instance := range []string{"space forbidden", "private/path", "private\\\\path", strings.Repeat("x", 129), "\u00e9"} {
		if parseFixture("HERDR_RELAY_INSTANCE_ID='"+instance+"'\n").Code() != ConfigInvalid { t.Fatal("invalid private identity admitted") }
	}
	t.Setenv("HERDR_RELAY_INSTANCE_ID", "ambient-instance-canary")
	if parseFixture("HERDR_RELAY_PORT=8375\n").instance != "" { t.Fatal("ambient identity merged or generated") }
	if parseFixture("HERDR_RELAY_INSTANCE_ID=one\nHERDR_RELAY_INSTANCE_ID=two\n").Code() != ConfigInvalid { t.Fatal("duplicate identity admitted") }
}

func TestLiteralConfigHerdrSocketOnlyExplicitPrivateLiteral(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/private/ambient-socket-canary")
	t.Setenv("HOME", "/private/ambient-home-canary")
	t.Setenv("XDG_RUNTIME_DIR", "/private/ambient-runtime-canary")
	for _, path := range []string{"/tmp/private/socket-canary.sock", "/tmp/owner's socket.sock", "/tmp/é.sock", "/"+strings.Repeat("x", maxHerdrSocketPath-1)} {
		raw := "'"+strings.ReplaceAll(path, "'", "'\\''")+"'"
		cfg := parseFixture("HERDR_SOCKET_PATH="+raw+"\n")
		if cfg.Code() != ConfigValid || cfg.Mode() != ModeCloudflare || cfg.herdrSocket != path { t.Fatal("selected literal socket lost or normalized") }
		if cfg.String() != "private diagnostic configuration" || cfg.GoString() != cfg.String() { t.Fatal("socket configuration reflected") }
		if _, err := json.Marshal(cfg); err == nil || strings.Contains(err.Error(), "socket-canary") { t.Fatal("socket configuration serialized") }
	}
	for _, path := range []string{"", "/", "relative.sock", "@abstract", "unix:/tmp/socket.sock", "http://127.0.0.1/socket", "/tmp/../socket.sock", "/tmp//socket.sock", "/tmp/socket.sock/", "/"+strings.Repeat("x", maxHerdrSocketPath), "/"+strings.Repeat("a/", maxHerdrParents)+"s"} {
		cfg := parseFixture("HERDR_SOCKET_PATH='"+path+"'\nHERDR_RELAY_PORT=18377\n")
		if cfg.Code() != ConfigValid || cfg.herdrSocket != "" || cfg.Mode() != ModeCloudflare || cfg.port != 18377 { t.Fatal("unsupported selector selected fallback or invalidated routing") }
	}
	if parseFixture("HERDR_RELAY_PORT=18377\n").herdrSocket != "" { t.Fatal("ambient socket discovered") }
	for _, text := range []string{"HERDR_SOCKET_PATH='/tmp/a'\nHERDR_SOCKET_PATH='/tmp/b'\n", "HERDR_SOCKET_PATH=$(discover)\n", "HERDR_SOCKET_PATH='$HOME/socket'\nBAD=$(discover)\n", "HERDR_SOCKET_PATH='/tmp/\x00.sock'\n", "HERDR_SOCKET_PATH='/tmp/\xff.sock'\n"} {
		if parseFixture(text).Code() != ConfigInvalid { t.Fatal("unsafe literal syntax or duplicate socket accepted") }
	}
}

func TestLiteralConfigModesAndConflicts(t *testing.T) {
	for _, mode := range []Mode{ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI} {
		cfg := fixtureConfig(mode)
		if cfg.Code() != ConfigValid || cfg.Mode() != mode || !cfg.tokenValid { t.Fatalf("mode fixture failed: %s", mode) }
	}
	legacy := parseFixture("HERDR_GATEWAY_URL='wss://one.example.test, ,wss://two.example.test/'\n")
	if legacy.Code() != ConfigValid || legacy.Mode() != ModeGateway || len(legacy.gateways) != 2 { t.Fatal("legacy gateway inference diverged") }
	for _, text := range []string{
		"HERDR_RELAY_TRANSPORT=unknown\n", "HERDR_RELAY_TRANSPORT=gateway\n",
		"HERDR_RELAY_TRANSPORT=cloudflare\nHERDR_GATEWAY_URL=wss://gateway.example.test\n",
		"HERDR_RELAY_TRANSPORT=tailscale-external\nHERDR_GATEWAY_URL=wss://gateway.example.test\n",
		"HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_TAILSCALE_ORIGIN=https://relay.example.test\n",
		"HERDR_GATEWAY_URL='wss://gateway.example.test,wss://gateway.example.test/'\n",
	} {
		if parseFixture(text).Code() != ConfigInvalid { t.Fatal("mode ambiguity/conflict was admitted") }
	}
}

func TestLiteralConfigRejectsExecutableSyntaxAndAmbiguity(t *testing.T) {
	for _, text := range []string{
		"K=$(touch /private/canary)\n", "K=`command`\n", "K='literal';command\n", "K='literal' >file\n",
		"K=\"$EXPANSION\"\n", "K=\"`command`\"\n", "K=foo\\ bar\n", "K='unterminated\n",
		"K='first\nsecond'\n", "K=value # suffix\n", "K='literal'&&command\n", "K=foo|bar\n",
		"K=first\nK=second\n", "bad=value\n", "1KEY=value\n", "K ='value'\n",
		"HERDR_RELAY_TOKEN=short\n", "K='secret\x1b[31m'\n", "#\x00\n", "K='\xff'\n", "K='\u0085'\n",
		"HERDR_RELAY_PORT=0\n", "HERDR_RELAY_PORT=65536\n", "HERDR_RELAY_PORT=08375\n", "HERDR_RELAY_PLUGIN_PORT=-1\n",
		"K="+strings.Repeat("a", MaxConfigValue+1)+"\n", strings.Repeat("A", 65)+"=value\n",
	} {
		cfg := parseFixture(text)
		if cfg.Code() != ConfigInvalid || cfg.Mode() != ModeUnknown { t.Fatal("unsafe config was not a sanitized refusal") }
	}
	var many strings.Builder
	for n := 0; n <= MaxConfigKeys; n++ { many.WriteString("K"+strings.Repeat("A", n/26)+string(rune('A'+n%26))+"=value\n") }
	if parseFixture(many.String()).Code() != ConfigInvalid { t.Fatal("key count was unbounded") }
}

func TestLiteralConfigOriginValidation(t *testing.T) {
	for _, origin := range []string{
		"wss://user:secret-canary@gateway.example.test", "wss://gateway.example.test/path",
		"wss://gateway.example.test?", "wss://gateway.example.test#", "wss://gateway.example.test/%zz",
		"wss://gateway.example.test:65536", "ws://remote.example.test", "wss://gateway.example.test\\suffix",
	} {
		if parseFixture("HERDR_GATEWAY_URL='"+origin+"'\n").Code() != ConfigInvalid { t.Fatal("unsafe gateway origin admitted") }
	}
	for _, origin := range []string{"wss://gateway.example.test", "wss://gateway.example.test:8443/", "ws://127.0.0.1:8375", "ws://[::1]:8375"} {
		if parseFixture("HERDR_GATEWAY_URL='"+origin+"'\n").Code() != ConfigValid { t.Fatal("valid gateway origin refused") }
	}
	for _, origin := range []string{"https://user:secret-canary@app.example.test", "http://app.example.test", "https://app.example.test?", "https://app.example.test#", "https://app.example.test/path"} {
		if parseFixture("HERDR_PHONE_APP_URL='"+origin+"'\n").Code() != ConfigInvalid { t.Fatal("unsafe app origin admitted") }
	}
}

type errorReader struct{}
func (errorReader) Read([]byte) (int, error) { return 0, errors.New("raw-secret/private/path error") }
func TestLiteralConfigCompleteInputBounds(t *testing.T) {
	exact := strings.Repeat("#"+strings.Repeat("a", MaxConfigLine-2)+"\n", MaxConfigBytes/MaxConfigLine)
	if len(exact) != MaxConfigBytes || parseFixture(exact).Code() != ConfigValid { t.Fatal("exact byte limit refused") }
	for _, input := range []string{exact+" ", exact+"!", exact+strings.Repeat(" ", MaxConfigBytes), "#"+strings.Repeat("a", MaxConfigLine)+"\n"} {
		if parseFixture(input).Code() != ConfigInvalid { t.Fatal("oversized valid prefix/suffix admitted") }
	}
	if ParseConfig(errorReader{}).Code() != ConfigInvalid || ParseConfig(nil).Code() != ConfigInvalid { t.Fatal("read failure was accepted") }
}

func privateFixture(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	path := filepath.Join(root, "relay.env")
	if err := os.WriteFile(path, []byte("HERDR_RELAY_PORT='8375'\n"), 0o600); err != nil { t.Fatal(err) }
	return root, path
}
func readConfigOnLinuxFixture(path string) Inspection {
	return readConfigWithHost(path, os.Geteuid(), "linux", openParent, openSelectedFile, func(r io.Reader) ([]byte, error) {
		return io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	})
}
func TestDarwinConfigAcquisitionRefusesBeforeFilesystemOpen(t *testing.T) {
	root, path := privateFixture(t)
	contents := "HERDR_RELAY_TOKEN='" + strings.Repeat("s", 32) + "'\nHERDR_RELAY_INSTANCE_ID=private-data-canary\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var beforeStat unix.Stat_t
	if err := unix.Stat(path, &beforeStat); err != nil || !privateRegular(&beforeStat, os.Geteuid()) {
		t.Fatal("config fixture was not apparently safe regular-file metadata")
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	parentOpens, contentOpens, reads := 0, 0, 0
	cfg := readConfigWithHost(path, os.Geteuid(), "darwin",
		func(path string) (int, string, error) { parentOpens++; return openParent(path) },
		func(parent int, base string) (int, error) { contentOpens++; return openSelectedFile(parent, base) },
		func(r io.Reader) ([]byte, error) { reads++; return io.ReadAll(r) })
	if cfg.Code() != ConfigUnavailable || cfg.Mode() != ModeUnknown || cfg.tokenPresent || cfg.tokenValid ||
		cfg.port != 0 || cfg.pluginPort != 0 || len(cfg.gateways) != 0 || cfg.appOrigin != "" ||
		cfg.relayOrigin != "" || cfg.instance != "" || cfg.herdrSocket != "" {
		t.Fatal("Darwin config refusal retained private input or returned a nonfixed result")
	}
	if parentOpens != 0 || contentOpens != 0 || reads != 0 {
		t.Fatal("Darwin config refusal crossed an acquisition boundary")
	}
	if cfg.String() != "private diagnostic configuration" || cfg.GoString() != cfg.String() {
		t.Fatal("Darwin refusal reflected private input")
	}
	if _, err := json.Marshal(cfg); err == nil || strings.Contains(err.Error(), "private-data-canary") {
		t.Fatal("Darwin refusal exposed private configuration")
	}
	if runtime.GOOS == "darwin" && ReadConfig(path).Code() != ConfigUnavailable {
		t.Fatal("production host policy did not refuse Darwin config")
	}

	report := Run(context.Background(), cfg, BuildIdentity{}, Options{}, Dependencies{})
	if report.ExitCode() != 3 {
		t.Fatal("unavailable early inputs became healthy or invalid invocation")
	}
	for _, check := range report.Checks() {
		if check.ID == CheckConfig || check.ID == CheckBuild || check.ID == CheckBundle {
			if check.State != Unknown || !check.Required {
				t.Fatal("unavailable early input weakened required report composition")
			}
		}
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) || !os.SameFile(beforeInfo, afterInfo) || beforeInfo.Mode() != afterInfo.Mode() ||
		!beforeInfo.ModTime().Equal(afterInfo.ModTime()) || !reflect.DeepEqual(beforeEntries, afterEntries) {
		t.Fatal("Darwin config refusal mutated fixture bytes, metadata or directory inventory")
	}
}

func TestSelectedFileReadDoesNotMutate(t *testing.T) {
	root, path := privateFixture(t)
	before, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	info, err := os.Stat(path)
	if err != nil { t.Fatal(err) }
	entries, err := os.ReadDir(root)
	if err != nil { t.Fatal(err) }
	for n := 0; n < 3; n++ { if readConfigOnLinuxFixture(path).Code() != ConfigValid { t.Fatal("private regular file unreadable") } }
	after, err := os.ReadFile(path)
	if err != nil { t.Fatal(err) }
	afterInfo, err := os.Stat(path)
	if err != nil { t.Fatal(err) }
	afterEntries, err := os.ReadDir(root)
	if err != nil { t.Fatal(err) }
	if !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || info.Mode() != afterInfo.Mode() || !reflect.DeepEqual(entries, afterEntries) {
		t.Fatal("diagnostic read changed bytes/identity/permissions/inventory")
	}
}

func TestSelectedFileRefusesUnsafePathsAndMetadata(t *testing.T) {
	root, path := privateFixture(t)
	if readConfigOnLinuxFixture(filepath.Join(root, "absent")).Code() != ConfigUnavailable { t.Fatal("absence inferred healthy config") }
	if readConfigOnLinuxFixture("relative.env").Code() != ConfigUnsafe { t.Fatal("relative selection admitted") }
	if err := os.Symlink(path, filepath.Join(root, "link")); err != nil { t.Fatal(err) }
	if readConfigOnLinuxFixture(filepath.Join(root, "link")).Code() != ConfigUnsafe { t.Fatal("file symlink followed") }
	if err := os.Symlink(root, filepath.Join(root, "parent-link")); err != nil { t.Fatal(err) }
	if readConfigOnLinuxFixture(filepath.Join(root, "parent-link", "relay.env")).Code() != ConfigUnsafe { t.Fatal("parent symlink followed") }
	if readConfigOnLinuxFixture(root).Code() != ConfigUnsafe { t.Fatal("directory admitted") }
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil { t.Fatal(err) }
	if readConfigOnLinuxFixture(fifo).Code() != ConfigUnsafe { t.Fatal("FIFO opened/read") }
	for _, perm := range []os.FileMode{0o644, 0o660, 0o000} {
		if err := os.Chmod(path, perm); err != nil { t.Fatal(err) }
		if readConfigOnLinuxFixture(path).Code() != ConfigUnsafe { t.Fatal("unsafe permissions accepted") }
	}
	if err := os.Chmod(path, 0o600); err != nil { t.Fatal(err) }
	if readConfigWithHost(path, os.Geteuid()+1, "linux", openParent, openSelectedFile, func(io.Reader) ([]byte, error) { t.Fatal("foreign owner read"); return nil, nil }).Code() != ConfigUnsafe {
		t.Fatal("owner mismatch accepted")
	}
	if readConfigWithHost(path, os.Geteuid(), "linux", openParent, openSelectedFile, func(io.Reader) ([]byte, error) { return nil, errors.New("raw-error-canary") }).Code() != ConfigUnavailable {
		t.Fatal("read failure not unavailable")
	}
}

func TestSelectedFileDoesNotDereferenceSecretsOrAcceptHardlinks(t *testing.T) {
	root, path := privateFixture(t)
	trap := filepath.Join(root, "token-fifo")
	if err := unix.Mkfifo(trap, 0o600); err != nil { t.Fatal(err) }
	text := "HERDR_GITHUB_TOKEN_FILE='"+trap+"'\nHERDR_RELAY_TOKEN='"+strings.Repeat("k", 32)+"'\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil { t.Fatal(err) }
	if readConfigOnLinuxFixture(path).Code() != ConfigValid { t.Fatal("literal private pointer caused a dereference/refusal") }
	if err := os.Link(path, filepath.Join(root, "hardlink")); err != nil { t.Fatal(err) }
	if readConfigOnLinuxFixture(path).Code() != ConfigUnsafe { t.Fatal("ambiguous hardlinked configuration accepted") }
}

func TestSelectedFileDetectsReplacementAndOversize(t *testing.T) {
	_, path := privateFixture(t)
	cfg := readConfigWithHost(path, os.Geteuid(), "linux", openParent, openSelectedFile, func(r io.Reader) ([]byte, error) {
		data, err := io.ReadAll(r)
		if err != nil { return nil, err }
		if err := os.Rename(path, path+".old"); err != nil { t.Fatal(err) }
		if err := os.WriteFile(path, data, 0o600); err != nil { t.Fatal(err) }
		return data, nil
	})
	if cfg.Code() != ConfigUnsafe { t.Fatal("replaced selected file accepted") }
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxConfigBytes+1)), 0o600); err != nil { t.Fatal(err) }
	if readConfigOnLinuxFixture(path).Code() != ConfigUnsafe { t.Fatal("oversized selected file read") }
}
