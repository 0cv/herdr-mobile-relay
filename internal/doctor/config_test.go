package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
	"golang.org/x/sys/unix"
)

func parseFixture(text string) Inspection { return ParseConfig(strings.NewReader(text)) }
func fixtureConfig(mode Mode) Inspection {
	text := "HERDR_RELAY_TOKEN='" + strings.Repeat("k", 32) + "'\nHERDR_RELAY_TRANSPORT='" + string(mode) + "'\n"
	switch mode {
	case ModeGateway:
		text += "HERDR_GATEWAY_URL='wss://gateway.example.test'\nHERDR_PHONE_APP_URL='https://app.example.test'\n"
	case ModeBYO:
		text += "HERDR_EXTERNAL_HTTPS_ORIGIN='https://relay.example.test'\n"
	case ModeManaged:
		text += "HERDR_TAILSCALE_ORIGIN='https://relay.example.test'\n"
	case ModeCLI:
		text += "HERDR_TAILSCALE_CLI_ORIGIN='https://relay.example.test:8443'\n"
	}
	return parseFixture(text)
}

func TestLiteralConfigGeneratedQuotingAndLegacy(t *testing.T) {
	for _, value := range []string{"", "simple", "owner's selected app", "''", "a'b'c", "$literal;not a command", "/private/user's config"} {
		// Exact common.sh shell_quote_value emission, not a shell execution.
		raw := "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
		got, ok := literal(raw)
		if !ok || got != value {
			t.Fatal("generated literal quoting did not round trip")
		}
		if parseFixture("UNKNOWN_KEY="+raw+"\n").Code() != ConfigValid {
			t.Fatal("benign unknown quoted key was refused")
		}
	}
	for _, input := range []string{"HERDR_RELAY_PORT=8375\n", "export HERDR_RELAY_PORT=\"8375\"\n", "# comment\nHERDR_RELAY_PORT='8375'\n"} {
		cfg := parseFixture(input)
		if cfg.Code() != ConfigValid || cfg.Mode() != ModeCloudflare || cfg.port != 8375 {
			t.Fatal("simple legacy literal failed")
		}
	}
	c := parseFixture("GH_TOKEN='secret-canary'\nHERDR_GITHUB_TOKEN_FILE='/private/token-canary'\nUNKNOWN_KEY='node-canary'\n")
	if c.Code() != ConfigValid || c.tokenPresent {
		t.Fatal("ambient/updater keys were merged into relay authentication")
	}
	if _, err := json.Marshal(c); err == nil {
		t.Fatal("private configuration was serializable")
	}
}

func TestLiteralConfigExpectedInstanceIsPrivateAndSelected(t *testing.T) {
	for _, instance := range []string{"", strings.Repeat("a", 32), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "private:instance@fixture", strings.Repeat("x", 128), "owner's-instance"} {
		raw := "'" + strings.ReplaceAll(instance, "'", "'\\''") + "'"
		cfg := parseFixture("HERDR_RELAY_INSTANCE_ID=" + raw + "\n")
		if cfg.Code() != ConfigValid || cfg.instance != instance {
			t.Fatal("bounded instance grammar/quoting diverged")
		}
		if cfg.String() != "private diagnostic configuration" || cfg.GoString() != cfg.String() {
			t.Fatal("private identity reflected")
		}
		if _, err := json.Marshal(cfg); err == nil || strings.Contains(err.Error(), "private:instance") {
			t.Fatal("identity serialized/reflected")
		}
	}
	for _, instance := range []string{"space forbidden", "private/path", "private\\\\path", strings.Repeat("x", 129), "\u00e9"} {
		if parseFixture("HERDR_RELAY_INSTANCE_ID='"+instance+"'\n").Code() != ConfigInvalid {
			t.Fatal("invalid private identity admitted")
		}
	}
	t.Setenv("HERDR_RELAY_INSTANCE_ID", "ambient-instance-canary")
	if parseFixture("HERDR_RELAY_PORT=8375\n").instance != "" {
		t.Fatal("ambient identity merged or generated")
	}
	if parseFixture("HERDR_RELAY_INSTANCE_ID=one\nHERDR_RELAY_INSTANCE_ID=two\n").Code() != ConfigInvalid {
		t.Fatal("duplicate identity admitted")
	}
}

func TestLiteralConfigHerdrSocketOnlyExplicitPrivateLiteral(t *testing.T) {
	t.Setenv("HERDR_SOCKET_PATH", "/private/ambient-socket-canary")
	t.Setenv("HOME", "/private/ambient-home-canary")
	t.Setenv("XDG_RUNTIME_DIR", "/private/ambient-runtime-canary")
	for _, path := range []string{"/tmp/private/socket-canary.sock", "/tmp/owner's socket.sock", "/tmp/é.sock", "/" + strings.Repeat("x", maxHerdrSocketPath-1)} {
		raw := "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
		cfg := parseFixture("HERDR_SOCKET_PATH=" + raw + "\n")
		if cfg.Code() != ConfigValid || cfg.Mode() != ModeCloudflare || cfg.herdrSocket != path {
			t.Fatal("selected literal socket lost or normalized")
		}
		if cfg.String() != "private diagnostic configuration" || cfg.GoString() != cfg.String() {
			t.Fatal("socket configuration reflected")
		}
		if _, err := json.Marshal(cfg); err == nil || strings.Contains(err.Error(), "socket-canary") {
			t.Fatal("socket configuration serialized")
		}
	}
	for _, path := range []string{"", "/", "relative.sock", "@abstract", "unix:/tmp/socket.sock", "http://127.0.0.1/socket", "/tmp/../socket.sock", "/tmp//socket.sock", "/tmp/socket.sock/", "/" + strings.Repeat("x", maxHerdrSocketPath), "/" + strings.Repeat("a/", maxHerdrParents) + "s"} {
		cfg := parseFixture("HERDR_SOCKET_PATH='" + path + "'\nHERDR_RELAY_PORT=18377\n")
		if cfg.Code() != ConfigValid || cfg.herdrSocket != "" || cfg.Mode() != ModeCloudflare || cfg.port != 18377 {
			t.Fatal("unsupported selector selected fallback or invalidated routing")
		}
	}
	if parseFixture("HERDR_RELAY_PORT=18377\n").herdrSocket != "" {
		t.Fatal("ambient socket discovered")
	}
	for _, text := range []string{"HERDR_SOCKET_PATH='/tmp/a'\nHERDR_SOCKET_PATH='/tmp/b'\n", "HERDR_SOCKET_PATH=$(discover)\n", "HERDR_SOCKET_PATH='$HOME/socket'\nBAD=$(discover)\n", "HERDR_SOCKET_PATH='/tmp/\x00.sock'\n", "HERDR_SOCKET_PATH='/tmp/\xff.sock'\n"} {
		if parseFixture(text).Code() != ConfigInvalid {
			t.Fatal("unsafe literal syntax or duplicate socket accepted")
		}
	}
}

func TestLiteralConfigModesAndConflicts(t *testing.T) {
	for _, mode := range []Mode{ModeCloudflare, ModeGateway, ModeManaged, ModeBYO, ModeCLI} {
		cfg := fixtureConfig(mode)
		if cfg.Code() != ConfigValid || cfg.Mode() != mode || !cfg.tokenValid {
			t.Fatalf("mode fixture failed: %s", mode)
		}
	}
	legacy := parseFixture("HERDR_GATEWAY_URL='wss://one.example.test, ,wss://two.example.test/'\n")
	if legacy.Code() != ConfigValid || legacy.Mode() != ModeGateway || len(legacy.gateways) != 2 {
		t.Fatal("legacy gateway inference diverged")
	}
	for _, text := range []string{
		"HERDR_RELAY_TRANSPORT=unknown\n", "HERDR_RELAY_TRANSPORT=gateway\n",
		"HERDR_RELAY_TRANSPORT=cloudflare\nHERDR_GATEWAY_URL=wss://gateway.example.test\n",
		"HERDR_RELAY_TRANSPORT=tailscale-external\nHERDR_GATEWAY_URL=wss://gateway.example.test\n",
		"HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_TAILSCALE_ORIGIN=https://relay.example.test\n",
		"HERDR_GATEWAY_URL='wss://gateway.example.test,wss://gateway.example.test/'\n",
	} {
		if parseFixture(text).Code() != ConfigInvalid {
			t.Fatal("mode ambiguity/conflict was admitted")
		}
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
		"K=" + strings.Repeat("a", MaxConfigValue+1) + "\n", strings.Repeat("A", 65) + "=value\n",
	} {
		cfg := parseFixture(text)
		if cfg.Code() != ConfigInvalid || cfg.Mode() != ModeUnknown {
			t.Fatal("unsafe config was not a sanitized refusal")
		}
	}
	var many strings.Builder
	for n := 0; n <= MaxConfigKeys; n++ {
		many.WriteString("K" + strings.Repeat("A", n/26) + string(rune('A'+n%26)) + "=value\n")
	}
	if parseFixture(many.String()).Code() != ConfigInvalid {
		t.Fatal("key count was unbounded")
	}
}

func TestLiteralConfigOriginValidation(t *testing.T) {
	for _, origin := range []string{
		"wss://user:secret-canary@gateway.example.test", "wss://gateway.example.test/path",
		"wss://gateway.example.test?", "wss://gateway.example.test#", "wss://gateway.example.test/%zz",
		"wss://gateway.example.test:65536", "ws://remote.example.test", "wss://gateway.example.test\\suffix",
	} {
		if parseFixture("HERDR_GATEWAY_URL='"+origin+"'\n").Code() != ConfigInvalid {
			t.Fatal("unsafe gateway origin admitted")
		}
	}
	for _, origin := range []string{"wss://gateway.example.test", "wss://gateway.example.test:8443/", "ws://127.0.0.1:8375", "ws://[::1]:8375"} {
		if parseFixture("HERDR_GATEWAY_URL='"+origin+"'\n").Code() != ConfigValid {
			t.Fatal("valid gateway origin refused")
		}
	}
	for _, origin := range []string{"https://user:secret-canary@app.example.test", "http://app.example.test", "https://app.example.test?", "https://app.example.test#", "https://app.example.test/path"} {
		if parseFixture("HERDR_PHONE_APP_URL='"+origin+"'\n").Code() != ConfigInvalid {
			t.Fatal("unsafe app origin admitted")
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("raw-secret/private/path error") }
func TestLiteralConfigCompleteInputBounds(t *testing.T) {
	exact := strings.Repeat("#"+strings.Repeat("a", MaxConfigLine-2)+"\n", MaxConfigBytes/MaxConfigLine)
	if len(exact) != MaxConfigBytes || parseFixture(exact).Code() != ConfigValid {
		t.Fatal("exact byte limit refused")
	}
	for _, input := range []string{exact + " ", exact + "!", exact + strings.Repeat(" ", MaxConfigBytes), "#" + strings.Repeat("a", MaxConfigLine) + "\n"} {
		if parseFixture(input).Code() != ConfigInvalid {
			t.Fatal("oversized valid prefix/suffix admitted")
		}
	}
	if ParseConfig(errorReader{}).Code() != ConfigInvalid || ParseConfig(nil).Code() != ConfigInvalid {
		t.Fatal("read failure was accepted")
	}
}

func privateFixture(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "relay.env")
	if err := os.WriteFile(path, []byte("HERDR_RELAY_PORT='8375'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, path
}
func readConfigOnLinuxFixture(path string) Inspection {
	return readConfigWithHost(path, os.Geteuid(), "linux", openParentWalk, openSelectedFile, func(r io.Reader) ([]byte, error) {
		return io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	})
}
func hostedDarwin() bool {
	return runtime.GOOS == "darwin" && os.Getenv("GITHUB_ACTIONS") == "true" &&
		os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted"
}

type aclAnswer struct {
	acl darwinacl.ACL
	err error
}

var noACL = aclAnswer{acl: darwinacl.ACL{Absent: true}}

// fakeACL scripts descriptor ACL evidence. Directories (the ancestor walk)
// answer by call number; the selected regular file answers by its own call
// number and must be the private regular-file descriptor.
type fakeACL struct {
	t                   *testing.T
	dirs                func(call int) aclAnswer
	files               []aclAnswer
	dirCalls, fileCalls int
}

func (f *fakeACL) inspect(fd int) (darwinacl.ACL, error) {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		f.t.Fatal("ACL inspection received an invalid descriptor")
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		f.dirCalls++
		answer := noACL
		if f.dirs != nil {
			answer = f.dirs(f.dirCalls - 1)
		}
		return answer.acl, answer.err
	}
	if !privateRegular(&st, os.Geteuid()) {
		f.t.Fatal("ACL inspection did not receive the selected regular-file descriptor")
	}
	answer := f.files[min(f.fileCalls, len(f.files)-1)]
	f.fileCalls++
	return answer.acl, answer.err
}

func readDarwinFixture(path string, f *fakeACL, during func()) (Inspection, int) {
	reads := 0
	cfg := readConfigWithACL(path, os.Geteuid(), "darwin", openParentWalk, openSelectedFile,
		func(r io.Reader) ([]byte, error) {
			reads++
			data, err := io.ReadAll(r)
			if during != nil {
				during()
			}
			return data, err
		}, f.inspect)
	return cfg, reads
}

func TestDarwinConfigAcquisitionRequiresDescriptorACLEvidence(t *testing.T) {
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

	parentOpens, contentOpens, reads, inspections := 0, 0, 0, 0
	cfg := readConfigWithACL(path, os.Geteuid(), "darwin",
		func(path string, visit func(int) error) (int, string, error) {
			parentOpens++
			return openParentWalk(path, visit)
		},
		func(parent int, base string) (int, error) { contentOpens++; return openSelectedFile(parent, base) },
		func(r io.Reader) ([]byte, error) { reads++; return io.ReadAll(r) },
		func(fd int) (darwinacl.ACL, error) { inspections++; return darwinacl.ACL{}, unix.ENOTSUP })
	if cfg.Code() != ConfigUnavailable || cfg.Mode() != ModeUnknown || cfg.tokenPresent || cfg.tokenValid ||
		cfg.port != 0 || cfg.pluginPort != 0 || len(cfg.gateways) != 0 || cfg.appOrigin != "" ||
		cfg.relayOrigin != "" || cfg.instance != "" || cfg.herdrSocket != "" {
		t.Fatal("Darwin config refusal retained private input or returned a nonfixed result")
	}
	// The walk stops at its first directory (the root), so no content is opened.
	if parentOpens != 1 || contentOpens != 0 || inspections != 1 || reads != 0 {
		t.Fatal("Darwin ACL uncertainty read private input or skipped descriptor inspection")
	}
	if cfg.String() != "private diagnostic configuration" || cfg.GoString() != cfg.String() {
		t.Fatal("Darwin refusal reflected private input")
	}
	if _, err := json.Marshal(cfg); err == nil || strings.Contains(err.Error(), "private-data-canary") {
		t.Fatal("Darwin refusal exposed private configuration")
	}
	if runtime.GOOS != "darwin" && readConfigWithHost(path, os.Geteuid(), "darwin",
		func(string, func(int) error) (int, string, error) {
			t.Fatal("simulated production Darwin opened parent")
			return -1, "", nil
		},
		openSelectedFile, func(io.Reader) ([]byte, error) { t.Fatal("simulated production Darwin read"); return nil, nil }).Code() != ConfigUnavailable {
		t.Fatal("simulated production Darwin admitted mode-only acquisition")
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

func TestDarwinDescriptorACLBeforeAndAfterRead(t *testing.T) {
	_, path := privateFixture(t)
	denyOnly := aclAnswer{acl: darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}}
	granting := aclAnswer{acl: darwinacl.ACL{Entries: 1}}
	unknown := aclAnswer{err: unix.ENOTSUP}
	for _, tc := range []struct {
		name   string
		files  []aclAnswer
		want   ConfigCode
		reads  int
		checks int
	}{
		{"absent", []aclAnswer{noACL, noACL}, ConfigValid, 1, 2},
		{"present", []aclAnswer{granting, noACL}, ConfigUnsafe, 0, 1},
		{"empty ACL", []aclAnswer{{}, noACL}, ConfigUnsafe, 0, 1},
		{"deny-only is for directories, not the file", []aclAnswer{denyOnly, noACL}, ConfigUnsafe, 0, 1},
		{"appeared", []aclAnswer{noACL, granting}, ConfigUnsafe, 1, 2},
		{"unknown before", []aclAnswer{unknown}, ConfigUnavailable, 0, 1},
		{"unknown after", []aclAnswer{noACL, unknown}, ConfigUnavailable, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeACL{t: t, files: tc.files}
			cfg, reads := readDarwinFixture(path, f, nil)
			if cfg.Code() != tc.want || reads != tc.reads || f.fileCalls != tc.checks {
				t.Fatal("descriptor ACL guard did not fail closed at the correct read boundary")
			}
		})
	}
}

func TestDarwinDescriptorSymlinkAndReplacementRefused(t *testing.T) {
	root, path := privateFixture(t)
	link := filepath.Join(root, "selected-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal("symlink fixture failed")
	}
	read := func(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
	plain := (&fakeACL{t: t, files: []aclAnswer{noACL}}).inspect
	if cfg := readConfigWithACL(link, os.Geteuid(), "darwin", openParentWalk, openSelectedFile, read, plain); cfg.Code() != ConfigUnsafe {
		t.Fatal("Darwin selection followed symlink")
	}
	cfg := readConfigWithACL(path, os.Geteuid(), "darwin", openParentWalk, openSelectedFile,
		func(r io.Reader) ([]byte, error) {
			data, err := io.ReadAll(r)
			if err != nil {
				return nil, err
			}
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal("replacement fixture failed")
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal("replacement fixture failed")
			}
			return data, nil
		}, plain)
	if cfg.Code() != ConfigUnsafe {
		t.Fatal("Darwin selection accepted replaced named file")
	}
}

// walkChain walks path with every directory plain and returns the inspected chain.
func walkChain(t *testing.T, path string) *ancestorChain {
	t.Helper()
	chain := &ancestorChain{uid: os.Geteuid(), inspect: (&fakeACL{t: t}).inspect}
	fd, _, err := openParentWalk(path, chain.visit)
	if err != nil {
		t.Fatal("fixture ancestor chain was not walkable")
	}
	_ = unix.Close(fd)
	return chain
}

func TestDarwinAncestorEvidenceRefusesBeforeContent(t *testing.T) {
	_, path := privateFixture(t)
	chain := walkChain(t, path)
	if len(chain.dirs) < 3 {
		t.Fatal("fixture ancestor chain was too short")
	}
	last := len(chain.dirs) - 1
	denyOnly := aclAnswer{acl: darwinacl.ACL{DenyOnly: true, Entries: 1, Denies: 1}}
	granting := aclAnswer{acl: darwinacl.ACL{Entries: 1}}
	at := func(call int, answer aclAnswer) func(int) aclAnswer {
		return func(n int) aclAnswer {
			if n == call {
				return answer
			}
			return noACL
		}
	}
	for _, tc := range []struct {
		name string
		dirs func(int) aclAnswer
		want ConfigCode
	}{
		{"home-like deny-only ACLs everywhere", func(int) aclAnswer { return denyOnly }, ConfigValid},
		{"granting root", at(0, granting), ConfigUnsafe},
		{"granting middle", at(1, granting), ConfigUnsafe},
		{"granting parent", at(last, granting), ConfigUnsafe},
		{"empty ACL", at(1, aclAnswer{}), ConfigUnsafe},
		{"unknown root", at(0, aclAnswer{err: unix.ENOTSUP}), ConfigUnavailable},
		{"unknown parent", at(last, aclAnswer{err: unix.ENOTSUP}), ConfigUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeACL{t: t, dirs: tc.dirs, files: []aclAnswer{noACL}}
			opens := 0
			cfg := readConfigWithACL(path, os.Geteuid(), "darwin", openParentWalk,
				func(parent int, base string) (int, error) { opens++; return openSelectedFile(parent, base) },
				func(r io.Reader) ([]byte, error) { return io.ReadAll(r) }, f.inspect)
			if cfg.Code() != tc.want {
				t.Fatal("ancestor ACL evidence produced the wrong verdict")
			}
			if tc.want == ConfigValid && f.dirCalls != 2*len(chain.dirs) {
				t.Fatal("ancestors were not proven before and after the read")
			}
			if tc.want != ConfigValid && (opens != 0 || f.fileCalls != 0) {
				t.Fatal("private content was opened below an unproven ancestor")
			}
		})
	}
}

func TestDarwinWritableAncestorRefusedBeforeContent(t *testing.T) {
	parent := func(root string) string { return root }
	grandparent := func(root string) string { return filepath.Dir(root) }
	for _, tc := range []struct {
		name  string
		which func(string) string
		mode  os.FileMode
	}{
		{"group writable parent", parent, 0o775},
		{"other writable parent", parent, 0o757},
		{"group writable grandparent", grandparent, 0o770},
		{"user owned sticky world writable grandparent", grandparent, 0o777 | os.ModeSticky},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, path := privateFixture(t)
			if err := os.Chmod(tc.which(root), tc.mode); err != nil {
				t.Fatal("ancestor mode fixture failed")
			}
			opens := 0
			cfg := readConfigWithACL(path, os.Geteuid(), "darwin", openParentWalk,
				func(parent int, base string) (int, error) { opens++; return openSelectedFile(parent, base) },
				func(r io.Reader) ([]byte, error) { return io.ReadAll(r) },
				(&fakeACL{t: t, files: []aclAnswer{noACL}}).inspect)
			if cfg.Code() != ConfigUnsafe || opens != 0 {
				t.Fatal("a writable ancestor was accepted or its content was opened")
			}
		})
	}
}

func TestDarwinAncestorsRewalkedAfterRead(t *testing.T) {
	granting := aclAnswer{acl: darwinacl.ACL{Entries: 1}}
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, root string)
		later  aclAnswer // the parent directory's evidence on the final rewalk
		want   ConfigCode
	}{
		{"unchanged", func(*testing.T, string) {}, noACL, ConfigValid},
		{"ancestor replaced by an identical directory", func(t *testing.T, root string) {
			base := filepath.Dir(root)
			t.Cleanup(func() { _ = os.RemoveAll(base + ".old") })
			if os.Rename(base, base+".old") != nil || os.Mkdir(base, 0o700) != nil ||
				os.Rename(filepath.Join(base+".old", filepath.Base(root)), root) != nil {
				t.Fatal("ancestor replacement fixture failed")
			}
		}, noACL, ConfigUnsafe},
		{"ancestor became group writable", func(t *testing.T, root string) {
			if os.Chmod(filepath.Dir(root), 0o770) != nil {
				t.Fatal("ancestor mode fixture failed")
			}
		}, noACL, ConfigUnsafe},
		{"ancestor gained a granting ACL", func(*testing.T, string) {}, granting, ConfigUnsafe},
		{"ancestor evidence became unknown", func(*testing.T, string) {}, aclAnswer{err: unix.ENOTSUP}, ConfigUnsafe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, path := privateFixture(t)
			final := 2*len(walkChain(t, path).dirs) - 1 // second walk, last directory
			f := &fakeACL{t: t, files: []aclAnswer{noACL}, dirs: func(call int) aclAnswer {
				if call == final {
					return tc.later
				}
				return noACL
			}}
			cfg, _ := readDarwinFixture(path, f, func() { tc.mutate(t, root) })
			if cfg.Code() != tc.want {
				t.Fatal("final ancestor rewalk did not prove the directory chain unchanged")
			}
		})
	}
}

func TestDarwinAncestorInspectionCannotBeBypassed(t *testing.T) {
	_, path := privateFixture(t)
	opens := 0
	cfg := readConfigWithACL(path, os.Geteuid(), "darwin",
		func(path string, _ func(int) error) (int, string, error) { return openParentWalk(path, nil) },
		func(parent int, base string) (int, error) { opens++; return openSelectedFile(parent, base) },
		func(r io.Reader) ([]byte, error) { return io.ReadAll(r) },
		(&fakeACL{t: t, files: []aclAnswer{noACL}}).inspect)
	if cfg.Code() != ConfigUnsafe || opens != 0 {
		t.Fatal("a parent walk that skipped the ancestor inspection was accepted")
	}
}

func ancestorStat(mode, uid uint32) *unix.Stat_t {
	st := &unix.Stat_t{Uid: uid}
	reflect.ValueOf(st).Elem().FieldByName("Mode").SetUint(uint64(mode)) // its width differs per OS
	return st
}

func TestDarwinAncestorModePolicy(t *testing.T) {
	const me = 501
	for _, tc := range []struct {
		name string
		mode uint32
		uid  uint32
		want bool
	}{
		{"private", unix.S_IFDIR | 0o700, me, true},
		{"readable", unix.S_IFDIR | 0o755, me, true},
		{"root owned", unix.S_IFDIR | 0o755, 0, true},
		{"root sticky temporary directory", unix.S_IFDIR | 0o1777, 0, true},
		{"group writable", unix.S_IFDIR | 0o775, me, false},
		{"other writable", unix.S_IFDIR | 0o757, me, false},
		{"root owned group writable", unix.S_IFDIR | 0o775, 0, false},
		{"user owned sticky world writable", unix.S_IFDIR | 0o1777, me, false},
		{"setgid", unix.S_IFDIR | 0o2755, me, false},
		{"setuid", unix.S_IFDIR | 0o4755, me, false},
		{"foreign owner", unix.S_IFDIR | 0o755, me + 1, false},
		{"unsearchable", unix.S_IFDIR | 0o600, me, false},
		{"not a directory", unix.S_IFREG | 0o755, me, false},
	} {
		if configAncestor(ancestorStat(tc.mode, tc.uid), me) != tc.want {
			t.Fatalf("%s: ancestor policy verdict changed", tc.name)
		}
	}
	// Identical to the established bundle policy for ancestors, for every owner.
	for _, mode := range []uint32{0o700, 0o755, 0o775, 0o757, 0o1777, 0o1700, 0o2755, 0o4755, 0o600, 0o300} {
		for _, uid := range []uint32{0, uint32(os.Geteuid()), uint32(os.Geteuid()) + 1} {
			st := ancestorStat(unix.S_IFDIR|mode, uid)
			if configAncestor(st, os.Geteuid()) != bundleDirectory(st, true) {
				t.Fatalf("mode %o owner class %d diverged from bundleDirectory", mode, min(uid, 2))
			}
		}
	}
}

// Plain private objects: the real descriptor route, with no ACL operation.
func TestDarwinRealNoACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("descriptor ACL evidence is Darwin only")
	}
	root, path := privateFixture(t)
	for _, name := range []string{root, path} {
		fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal("plain fixture could not be opened")
		}
		acl, err := darwinacl.Inspect(fd)
		_ = unix.Close(fd)
		if err != nil || acl != (darwinacl.ACL{Absent: true}) {
			t.Fatal("plain private object did not report absent ACL evidence")
		}
	}
	if _, err := darwinacl.Inspect(-1); err == nil {
		t.Fatal("an invalid descriptor produced ACL evidence")
	}
	if cfg := ReadConfig(path); cfg.Code() != ConfigValid {
		t.Fatalf("plain private config in plain private directories was not admitted; ancestors (owner:mode:acl): %s", darwinChainSummary(path))
	}
}

// darwinChainSummary lists each ancestor as owner class, mode bits and ACL kind
// with entry count: enough to diagnose a refused chain on a runner without a
// path, uid, GUID or ACL identity.
func darwinChainSummary(path string) string {
	var chain []string
	fd, _, err := openParentWalk(path, func(fd int) error {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil {
			return errAncestorUnknown
		}
		owner := "other"
		if st.Uid == 0 {
			owner = "root"
		} else if int(st.Uid) == os.Geteuid() {
			owner = "self"
		}
		kind := "error"
		if acl, err := darwinacl.Inspect(fd); err == nil {
			kind = "other"
			if acl.Absent {
				kind = "absent"
			} else if acl.DenyOnly {
				kind = "deny-only"
			}
			kind += "/" + strconv.Itoa(acl.Entries)
		}
		chain = append(chain, owner+":"+strconv.FormatUint(uint64(st.Mode&0o7777), 8)+":"+kind)
		return nil
	})
	if err == nil {
		_ = unix.Close(fd)
	}
	return strings.Join(chain, " ")
}

func TestDarwinHostedRealACLGrant(t *testing.T) {
	if !hostedDarwin() {
		t.Skip("real ACL grant is confined to disposable hosted Darwin runners")
	}
	_, path := privateFixture(t) // benign contents in isolated scratch, no owner configuration
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-N", path).Run() })
	// chmod(1)'s documented +a syntax *creates* the grant; the only ACL
	// inspection below is the production descriptor route, never ls/shell.
	if err := exec.Command("/bin/chmod", "+a", "group:everyone allow read", path).Run(); err != nil {
		t.Fatal("hosted ACL fixture creation failed")
	}
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil || !privateRegular(&st, os.Geteuid()) {
		t.Fatal("ACL fixture changed ordinary private mode/identity")
	}
	if cfg := ReadConfig(path); cfg.Code() != ConfigUnsafe || cfg.Mode() != ModeUnknown {
		t.Fatal("actual descriptor ACL grant was not refused")
	}
}

func TestDarwinHostedRealDirectoryACL(t *testing.T) {
	if !hostedDarwin() {
		t.Skip("real directory ACLs are confined to disposable hosted Darwin runners")
	}
	for _, tc := range []struct {
		name, acl string
		want      ConfigCode
	}{
		{"home-like deny only", "group:everyone deny delete", ConfigValid},
		{"allow entry", "user:nobody allow add_file,delete_child", ConfigUnsafe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, path := privateFixture(t)
			t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-N", root).Run() }) // deny delete blocks removal
			if err := exec.Command("/bin/chmod", "+a", tc.acl, root).Run(); err != nil {
				t.Fatal("hosted ACL fixture creation failed")
			}
			if cfg := ReadConfig(path); cfg.Code() != tc.want {
				t.Fatal("actual directory ACL verdict changed")
			}
		})
	}
}

func TestSelectedFileReadDoesNotMutate(t *testing.T) {
	root, path := privateFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		if readConfigOnLinuxFixture(path).Code() != ConfigValid {
			t.Fatal("private regular file unreadable")
		}
	}
	after, err := os.ReadFile(path)
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
	if !bytes.Equal(before, after) || !os.SameFile(info, afterInfo) || info.Mode() != afterInfo.Mode() || !reflect.DeepEqual(entries, afterEntries) {
		t.Fatal("diagnostic read changed bytes/identity/permissions/inventory")
	}
}

func TestSelectedFileRefusesUnsafePathsAndMetadata(t *testing.T) {
	root, path := privateFixture(t)
	if readConfigOnLinuxFixture(filepath.Join(root, "absent")).Code() != ConfigUnavailable {
		t.Fatal("absence inferred healthy config")
	}
	if readConfigOnLinuxFixture("relative.env").Code() != ConfigUnsafe {
		t.Fatal("relative selection admitted")
	}
	if err := os.Symlink(path, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if readConfigOnLinuxFixture(filepath.Join(root, "link")).Code() != ConfigUnsafe {
		t.Fatal("file symlink followed")
	}
	if err := os.Symlink(root, filepath.Join(root, "parent-link")); err != nil {
		t.Fatal(err)
	}
	if readConfigOnLinuxFixture(filepath.Join(root, "parent-link", "relay.env")).Code() != ConfigUnsafe {
		t.Fatal("parent symlink followed")
	}
	if readConfigOnLinuxFixture(root).Code() != ConfigUnsafe {
		t.Fatal("directory admitted")
	}
	fifo := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if readConfigOnLinuxFixture(fifo).Code() != ConfigUnsafe {
		t.Fatal("FIFO opened/read")
	}
	for _, perm := range []os.FileMode{0o644, 0o660, 0o000} {
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
		if readConfigOnLinuxFixture(path).Code() != ConfigUnsafe {
			t.Fatal("unsafe permissions accepted")
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if readConfigWithHost(path, os.Geteuid()+1, "linux", openParentWalk, openSelectedFile, func(io.Reader) ([]byte, error) { t.Fatal("foreign owner read"); return nil, nil }).Code() != ConfigUnsafe {
		t.Fatal("owner mismatch accepted")
	}
	if readConfigWithHost(path, os.Geteuid(), "linux", openParentWalk, openSelectedFile, func(io.Reader) ([]byte, error) { return nil, errors.New("raw-error-canary") }).Code() != ConfigUnavailable {
		t.Fatal("read failure not unavailable")
	}
}

func TestSelectedFileDoesNotDereferenceSecretsOrAcceptHardlinks(t *testing.T) {
	root, path := privateFixture(t)
	trap := filepath.Join(root, "token-fifo")
	if err := unix.Mkfifo(trap, 0o600); err != nil {
		t.Fatal(err)
	}
	text := "HERDR_GITHUB_TOKEN_FILE='" + trap + "'\nHERDR_RELAY_TOKEN='" + strings.Repeat("k", 32) + "'\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if readConfigOnLinuxFixture(path).Code() != ConfigValid {
		t.Fatal("literal private pointer caused a dereference/refusal")
	}
	if err := os.Link(path, filepath.Join(root, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if readConfigOnLinuxFixture(path).Code() != ConfigUnsafe {
		t.Fatal("ambiguous hardlinked configuration accepted")
	}
}

func TestSelectedFileDetectsReplacementAndOversize(t *testing.T) {
	_, path := privateFixture(t)
	cfg := readConfigWithHost(path, os.Geteuid(), "linux", openParentWalk, openSelectedFile, func(r io.Reader) ([]byte, error) {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return data, nil
	})
	if cfg.Code() != ConfigUnsafe {
		t.Fatal("replaced selected file accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxConfigBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if readConfigOnLinuxFixture(path).Code() != ConfigUnsafe {
		t.Fatal("oversized selected file read")
	}
}
