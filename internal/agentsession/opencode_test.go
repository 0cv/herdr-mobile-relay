package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCodePayload(t *testing.T) {
	for _, tc := range []struct{ data, want string }{{`{"session_id":"ses_root"}`, "ses_root"}, {`{"session_id":4}`, ""}, {`{}`, ""}, {`invalid`, ""}} {
		if got := session("opencode", []byte(tc.data), func(string) string { return "" }); got != tc.want {
			t.Fatalf("%s: %q", tc.data, got)
		}
	}
}

func TestOpenCodePluginLifecycle(t *testing.T) {
	home := t.TempDir()
	var out bytes.Buffer
	run := func(op string) {
		t.Helper()
		if err := manageOpenCodePlugin(home, `/relay's "binary"`, op, &out); err != nil {
			t.Fatal(err)
		}
	}
	run("install")
	dir := filepath.Join(home, ".config", "opencode")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("created absent config")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "cli.json")
	original := []byte(`{"plugins":["./herdr-opencode"],"theme":"test"}`)
	if err := os.WriteFile(config, original, 0600); err != nil {
		t.Fatal(err)
	}
	run("install")
	path := filepath.Join(dir, "herdr-mobile-relay-session", "tui.js")
	plugin := fileBytes(t, path)
	for _, want := range []string{"herdr-tui-session.js (Apache-2.0)", string(raw(`/relay's "binary"`)), "JSON.stringify({ session_id: id })", "if (!process.env.HC_TERMINAL_SOCKET) return;"} {
		if !strings.Contains(string(plugin), want) {
			t.Fatalf("missing %s", want)
		}
	}
	installed := fileBytes(t, config)
	wantConfig := "{\n  \"plugins\": [\n    \"./herdr-opencode\",\n    \"./herdr-mobile-relay-session\"\n  ],\n  \"theme\": \"test\"\n}\n"
	if string(installed) != wantConfig {
		t.Fatalf("config = %s", installed)
	}
	out.Reset()
	run("status")
	if out.String() != "opencode: installed\n" {
		t.Fatal(out.String())
	}
	run("install")
	if !bytes.Equal(installed, fileBytes(t, config)) || !bytes.Equal(plugin, fileBytes(t, path)) {
		t.Fatal("not idempotent")
	}
	run("uninstall")
	run("uninstall")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plugin remains")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("plugin directory remains")
	}
	root, err := parseObject(fileBytes(t, config))
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	if err := json.Unmarshal(root.get("plugins"), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != "./herdr-opencode" || string(root.get("theme")) != `"test"` {
		t.Fatal(string(fileBytes(t, config)))
	}
	for _, commented := range [][]byte{[]byte("{\n// Herdr\n\"plugins\": []\n}"), []byte(`{"plugins":[}`)} {
		if err := os.WriteFile(config, commented, 0600); err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"install", "uninstall"} {
			if err := manageOpenCodePlugin(home, "/relay", op, &out); err == nil || !strings.Contains(err.Error(), "strict JSON") {
				t.Fatalf("expected refusal: %v", err)
			}
			if !bytes.Equal(commented, fileBytes(t, config)) {
				t.Fatal("comments changed")
			}
		}
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	run("install")
	if got := string(fileBytes(t, config)); got != "{\n  \"plugins\": [\n    \"./herdr-mobile-relay-session\"\n  ]\n}\n" {
		t.Fatal(got)
	}
	run("uninstall")
}

func TestOpenCodePluginNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "relay stub")
	if err := os.WriteFile(stub, []byte("#!"+node+"\nconst fs = require('node:fs'); if (process.argv.slice(2).join(' ') !== 'agent-session opencode') process.exit(1); fs.appendFileSync(process.env.RECORD, fs.readFileSync(0, 'utf8') + '\\n');\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".config", "opencode"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := manageOpenCodePlugin(dir, stub, "install", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	script := `import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import plugin from './.config/opencode/herdr-mobile-relay-session/tui.js';
test('selection, retries, guard and disposal', async () => {
  assert.equal(plugin.id, 'herdr-mobile-relay.opencode.session-selection');
  delete process.env.HC_TERMINAL_SOCKET;
  assert.equal(plugin.setup({}), undefined);
  process.env.HC_TERMINAL_SOCKET = 'fake';
  let route = {type:'session', sessionID:'child'};
  const sessions = new Map([['child',{parentID:'root'}]]);
  const dispose = plugin.setup({ui:{router:{current:()=>route}},data:{session:{get:id=>sessions.get(id)}}});
  const wait = () => new Promise(resolve=>setTimeout(resolve,1000));
  const payloads = () => { try { return readFileSync(process.env.RECORD,'utf8').trim().split('\n').map(JSON.parse); } catch { return []; } };
  try {
    await wait(); assert.deepEqual(payloads(),[]);
    route = {type:'session',sessionID:'other'};
    sessions.set('root',{}); sessions.set('other',{});
    await wait(); assert.deepEqual(payloads(),[{session_id:'other'}]);
    route = {type:'session',sessionID:'child'};
    await wait(); assert.deepEqual(payloads(),[{session_id:'other'},{session_id:'root'}]);
    route = {type:'session',sessionID:'root'};
    await wait(); assert.equal(payloads().length,2);
    route = {type:'home'}; await wait();
    route = {type:'session',sessionID:'root'}; await wait();
    assert.equal(payloads().length,3);
  } finally { dispose(); }
  route = {type:'session',sessionID:'other'}; await wait();
  assert.equal(payloads().length,3);
});
`
	testPath := filepath.Join(dir, "plugin.test.mjs")
	if err := os.WriteFile(testPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--test", testPath)
	cmd.Env = append(os.Environ(), "RECORD="+filepath.Join(dir, "payloads"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
}
