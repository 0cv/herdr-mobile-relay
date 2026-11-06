package agentsession

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
)

const opencodePluginName = "./herdr-mobile-relay-session"

func opencodePlugin(binary string) []byte {
	return []byte(fmt.Sprintf(`// Session selection adapted from Herdr's herdr-tui-session.js (Apache-2.0).
import { spawn } from "node:child_process";
export default {
  id: "herdr-mobile-relay.opencode.session-selection",
  setup(api) {
    if (!process.env.HC_TERMINAL_SOCKET) return;
    let selected;
    function sync() {
      try {
        const route = api.ui.router.current();
        let id = route.type === "session" ? route.sessionID : undefined;
        const seen = new Set();
        while (typeof id === "string" && id && !seen.has(id)) {
          seen.add(id);
          const session = api.data.session.get(id);
          if (!session) return;
          if (session.parentID) {
            id = session.parentID;
            continue;
          }
          if (id === selected) return;
          selected = id;
          const child = spawn(%s, ["agent-session", "opencode"], { stdio: ["pipe", "ignore", "ignore"] });
          child.on("error", () => {});
          child.stdin.on("error", () => {});
          child.stdin.end(JSON.stringify({ session_id: id }));
          return;
        }
        selected = undefined;
      } catch {}
    }
    sync();
    const poll = setInterval(sync, 100);
    return () => clearInterval(poll);
  },
};
`, raw(binary)))
}

func manageOpenCodePlugin(home, binary, operation string, output io.Writer) error {
	dir := filepath.Join(home, ".config", "opencode")
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		fmt.Fprintln(output, "opencode: config absent")
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	path := filepath.Join(dir, "cli.json")
	data, err := os.ReadFile(path)
	absent := os.IsNotExist(err)
	if err != nil && !absent {
		return err
	}
	mode := os.FileMode(0600)
	if absent {
		data = []byte("{}")
	} else {
		info, err = os.Stat(path)
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
	}
	root, err := parseObject(data)
	if err != nil {
		return fmt.Errorf("%s: refusing to edit: only strict JSON is supported; comments and trailing commas must be removed manually: %w", path, err)
	}
	entries := []json.RawMessage{}
	if value := root.get("plugins"); value != nil {
		if len(value) == 0 || value[0] != '[' {
			return fmt.Errorf("%s: plugins must be an array", path)
		}
		if err := json.Unmarshal(value, &entries); err != nil {
			return err
		}
	}
	kept := make([]json.RawMessage, 0, len(entries)+1)
	found := false
	for _, entry := range entries {
		var name string
		if json.Unmarshal(entry, &name) == nil && name == opencodePluginName {
			found = true
		} else {
			kept = append(kept, entry)
		}
	}
	pluginDir := filepath.Join(dir, "herdr-mobile-relay-session")
	pluginPath := filepath.Join(pluginDir, "tui.js")
	state := "not installed"
	if found {
		if _, err := os.Stat(pluginPath); err == nil {
			state = "installed"
		}
	}
	if operation == "install" {
		if err := os.MkdirAll(pluginDir, 0700); err != nil {
			return err
		}
		if _, err := atomicfile.Replace(pluginDir, pluginPath, ".agent-hooks-*", opencodePlugin(binary), 0600); err != nil {
			return err
		}
		kept = append(kept, raw(opencodePluginName))
		state = "installed"
	}
	if operation == "install" || operation == "uninstall" && found {
		root.set("plugins", raw(kept))
		var out bytes.Buffer
		if err := json.Indent(&out, raw(root), "", "  "); err != nil {
			return err
		}
		out.WriteByte('\n')
		if !bytes.Equal(data, out.Bytes()) {
			if _, err := atomicfile.Replace(dir, path, ".agent-hooks-*", out.Bytes(), mode); err != nil {
				return err
			}
		}
	}
	if operation == "uninstall" {
		if err := os.Remove(pluginPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		entries, err := os.ReadDir(pluginDir)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && len(entries) == 0 {
			if err := os.Remove(pluginDir); err != nil {
				return err
			}
		}
		state = "not installed"
	}
	fmt.Fprintf(output, "opencode: %s\n", state)
	return nil
}
