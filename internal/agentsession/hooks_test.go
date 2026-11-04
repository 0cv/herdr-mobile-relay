package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, home, agent, data string) string {
	t.Helper()
	name := "hooks.json"
	if agent == "claude" {
		name = "settings.json"
	}
	dir := filepath.Join(home, "."+agent)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHooksLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			matcher := ""
			if agent == "claude" {
				matcher = `"matcher":"^(startup|resume|clear|compact|fork)$",`
			}
			original := `{"z":1,"hooks":{"Stop":[],"SessionStart":[{` + matcher + `"hooks":[{"command":"bash '/home/test/.` + agent + `/herdr-agent-state.sh' session && echo ready > /dev/null < /dev/null","timeout":10,"type":"command"}]}]},"a":{"z":2,"a":1}}`
			path := fixture(t, home, agent, original)
			run := func(op, binary string) string {
				t.Helper()
				var out bytes.Buffer
				binary = filepath.Join(home, filepath.Base(binary))
				if err := os.WriteFile(binary, nil, 0700); err != nil {
					t.Fatal(err)
				}
				if err := manageHooks(home, binary, []string{t.TempDir()}, op, &out); err != nil {
					t.Fatal(err)
				}
				state := "installed"
				if op == "uninstall" {
					state = "not installed"
				}
				lines := 2
				if op == "install" && strings.Contains(out.String(), "codex: installed\n") {
					lines++
					if !strings.HasSuffix(out.String(), "codex: start Codex with --no-daemon in herdr.app terminals; its Conversation appears after the first prompt\n") {
						t.Fatalf("missing Codex guidance: %q", out.String())
					}
				}
				if !strings.Contains(out.String(), agent+": "+state+"\n") || strings.Count(out.String(), "\n") != lines {
					t.Fatalf("feedback: %q", out.String())
				}
				return out.String()
			}
			run("install", "/old relay")
			first := fileBytes(t, path)
			if !bytes.Contains(first, []byte("&& echo ready > /dev/null < /dev/null")) {
				t.Fatalf("Herdr command rewritten: %s", first)
			}
			run("install", "/old relay")
			if !bytes.Equal(first, fileBytes(t, path)) {
				t.Fatal("install is not idempotent")
			}
			run("install", "/new relay's binary")
			data := fileBytes(t, path)
			if strings.Count(string(data), "agent-session "+agent) != 1 || bytes.Contains(data, []byte("/old relay")) {
				t.Fatalf("replacement: %s", data)
			}
			if !strings.Contains(run("status", "unused"), agent+": installed\n") {
				t.Fatal("status")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatal("permissions changed", err)
			}
			run("uninstall", "unused")
			var expected bytes.Buffer
			if err := json.Indent(&expected, []byte(original), "", "  "); err != nil {
				t.Fatal(err)
			}
			expected.WriteByte('\n')
			if !bytes.Equal(fileBytes(t, path), expected.Bytes()) {
				t.Fatalf("original order/content not restored: %s", fileBytes(t, path))
			}
			before := fileBytes(t, path)
			run("uninstall", "unused")
			if !bytes.Equal(before, fileBytes(t, path)) {
				t.Fatal("uninstall changed unrelated content")
			}
		})
	}
}

func TestHooksAbsentAndMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	if err := Hooks("status", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "claude: config absent\ncodex: config absent\n" {
		t.Fatal(out.String())
	}
	binary := filepath.Join(home, "relay")
	if err := os.WriteFile(binary, nil, 0700); err != nil {
		t.Fatal(err)
	}
	if err := manageHooks(home, binary, []string{t.TempDir()}, "install", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("created absent directory")
	}
	claude := fixture(t, home, "claude", `{"z":1}`)
	codex := fixture(t, home, "codex", `{"hooks":`)
	if err := manageHooks(home, binary, []string{t.TempDir()}, "install", &out); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatal(err)
	}
	if string(fileBytes(t, claude)) != `{"z":1}` || string(fileBytes(t, codex)) != `{"hooks":` {
		t.Fatal("malformed configuration caused a write")
	}
}

func TestHooksEmptyAndShared(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, original := range []string{`{}`, `{"hooks":{"Stop":[]}}`, `{"hooks":{},"a":1}`, `{"hooks":{"SessionStart":[]}}`, `{"hooks":{"SessionStart":null}}`} {
		installed, _, err := editHooks([]byte(original), "codex", "/relay", true)
		if err != nil {
			t.Fatal(err)
		}
		removed, found, err := editHooks(installed, "codex", "", false)
		if err != nil || !found {
			t.Fatal(err)
		}
		if bytes.Contains(removed, []byte("SessionStart")) {
			t.Fatal(string(removed))
		}
	}
	original := `{"hooks":{"SessionStart":[{"matcher":"x","hooks":[{"type":"command","command":"'/old' agent-session codex"},{"type":"command","command":"echo keep"}]}]}}`
	removed, found, err := editHooks([]byte(original), "codex", "", false)
	if err != nil || !found || !bytes.Contains(removed, []byte("echo keep")) || !bytes.Contains(removed, []byte(`"matcher": "x"`)) {
		t.Fatalf("%s %v", removed, err)
	}
	for _, bad := range []string{`null`, `{} {}`, `{"hooks":null}`, `{"hooks":{"SessionStart":{}}}`, `{"a":1,"a":2}`} {
		if _, _, err := editHooks([]byte(bad), "codex", "/relay", true); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestHooksTemporaryBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	temp := filepath.Join(root, "tmp")
	if err := os.Mkdir(temp, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(temp, "relay")
	if err := os.WriteFile(binary, nil, 0700); err != nil {
		t.Fatal(err)
	}
	binaryLink := filepath.Join(root, "relay-link")
	tempLink := filepath.Join(root, "tmp-link")
	for link, target := range map[string]string{binaryLink: binary, tempLink: temp} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	claude := fixture(t, home, "claude", `{}`)
	codex := fixture(t, home, "codex", `{}`)
	for _, executable := range []string{binary, binaryLink} {
		for _, dir := range []string{temp, tempLink} {
			var out bytes.Buffer
			err := manageHooks(home, executable, []string{dir}, "install", &out)
			if err == nil || !strings.Contains(err.Error(), "is a temporary go run build; run install from an installed or built herdr-mobile-relay binary") {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.Len() != 0 || string(fileBytes(t, claude)) != `{}` || string(fileBytes(t, codex)) != `{}` {
				t.Fatal("temporary binary caused a write")
			}
			for _, operation := range []string{"status", "uninstall"} {
				if err := manageHooks(home, executable, []string{dir}, operation, &out); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	outside := temp + "-relay"
	if err := os.WriteFile(outside, nil, 0700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := manageHooks(home, outside, []string{tempLink}, "install", &out); err != nil {
		t.Fatalf("binary outside temporary directory rejected: %v", err)
	}
}

func TestHooksBuildCacheBinary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	cache := filepath.Join(root, "go-build")
	binary := filepath.Join(cache, "a0", "hash-d", "herdr-mobile-relay")
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, nil, 0700); err != nil {
		t.Fatal(err)
	}
	claude := fixture(t, home, "claude", "{}")
	codex := fixture(t, home, "codex", "{}")
	var out bytes.Buffer
	err := manageHooks(home, binary, []string{filepath.Join(root, "missing"), t.TempDir(), cache}, "install", &out)
	if err == nil || !strings.Contains(err.Error(), "is a temporary go run build; run install from an installed or built herdr-mobile-relay binary") {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Len() != 0 || string(fileBytes(t, claude)) != "{}" || string(fileBytes(t, codex)) != "{}" {
		t.Fatal("build-cache binary caused a write")
	}
}

func TestHooksStatusNotInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fixture(t, home, "claude", `{}`)
	fixture(t, home, "codex", `{}`)
	var out bytes.Buffer
	if err := Hooks("status", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "claude: not installed\ncodex: not installed\n" {
		t.Fatal(out.String())
	}
}
