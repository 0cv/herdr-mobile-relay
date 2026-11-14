package agentsession

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
	"github.com/0cv/herdr-mobile-relay/internal/config"
)

type field struct {
	key   string
	value json.RawMessage
}
type object []field

func parseObject(data []byte) (object, error) {
	if !json.Valid(data) {
		return nil, fmt.Errorf("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	var result object
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, err
		}
		key := token.(string)
		if result.get(key) != nil {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		result = append(result, field{key, value})
	}
	return result, nil
}

func (o object) get(key string) json.RawMessage {
	for _, f := range o {
		if f.key == key {
			return f.value
		}
	}
	return nil
}

func (o *object) set(key string, value json.RawMessage) {
	for i, f := range *o {
		if f.key != key {
			continue
		}
		if value == nil {
			*o = append((*o)[:i], (*o)[i+1:]...)
		} else {
			(*o)[i].value = value
		}
		return
	}
	if value != nil {
		*o = append(*o, field{key, value})
	}
}

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key := raw(f.key)
		b.Write(key)
		b.WriteByte(':')
		b.Write(f.value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func raw(v any) json.RawMessage {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type hookAgent struct {
	name        string
	dir         string
	file        string
	event       string
	rootKey     string
	flat        bool
	version     int
	matcher     string
	installNote string
}

var hookAgents = []hookAgent{
	{name: "claude", dir: ".claude", file: "settings.json", event: "SessionStart", matcher: "^(startup|resume|clear|compact|fork)$"},
	{
		name: "codex", dir: ".codex", file: "hooks.json", event: "SessionStart",
		installNote: "codex: start Codex with --no-daemon in herdr.app terminals; its Conversation appears after the first prompt",
	},
	{name: "cursor", dir: ".cursor", file: "hooks.json", event: "sessionStart", flat: true, version: 1},
	{name: "qoder", dir: ".qoder", file: "settings.json", event: "SessionStart"},
	{name: "agy", dir: filepath.Join(".gemini", "config"), file: "hooks.json", event: "SessionStart", rootKey: "herdr-mobile-relay"},
}

func editHooks(data []byte, agent hookAgent, binary string, install bool) ([]byte, bool, error) {
	root, err := parseObject(data)
	if err != nil {
		return nil, false, err
	}
	command := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "' agent-session " + agent.name
	if agent.rootKey != "" {
		found := root.get(agent.rootKey) != nil
		if install {
			root.set(agent.rootKey, raw(object{{agent.event, raw([]hookCommand{{"command", command, 5}})}}))
		} else if found {
			root.set(agent.rootKey, nil)
		} else {
			return data, false, nil
		}
		return formatHooks(root, found)
	}
	var hooks object
	if value := root.get("hooks"); value != nil {
		hooks, err = parseObject(value)
		if err != nil {
			return nil, false, fmt.Errorf("hooks: %w", err)
		}
	}
	if install && agent.version != 0 && root.get("version") == nil {
		root.set("version", raw(agent.version))
	}
	var entries []json.RawMessage
	if value := hooks.get(agent.event); value != nil {
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, false, err
		}
	}
	suffix := " agent-session " + agent.name
	found := false
	kept := make([]json.RawMessage, 0, len(entries)+1)
	for _, entry := range entries {
		obj, err := parseObject(entry)
		if err != nil {
			return nil, false, fmt.Errorf("SessionStart entry: %w", err)
		}
		if agent.flat {
			var command string
			if value := obj.get("command"); value != nil {
				if err := json.Unmarshal(value, &command); err != nil {
					return nil, false, err
				}
			}
			if strings.HasSuffix(strings.TrimSpace(command), suffix) {
				found = true
			} else {
				kept = append(kept, entry)
			}
			continue
		}
		var commands []json.RawMessage
		if value := obj.get("hooks"); value != nil {
			if err := json.Unmarshal(value, &commands); err != nil {
				return nil, false, err
			}
		}
		remaining := make([]json.RawMessage, 0, len(commands))
		removed := false
		for _, command := range commands {
			var hook hookCommand
			if err := json.Unmarshal(command, &hook); err != nil {
				return nil, false, err
			}
			if hook.Type == "command" && strings.HasSuffix(strings.TrimSpace(hook.Command), suffix) {
				found = true
				removed = true
				continue
			}
			remaining = append(remaining, command)
		}
		if !removed {
			kept = append(kept, entry)
			continue
		}
		if len(remaining) > 0 {
			obj.set("hooks", raw(remaining))
			kept = append(kept, raw(obj))
		}
	}
	if install {
		entry := object{}
		if agent.matcher != "" {
			entry.set("matcher", raw(agent.matcher))
		}
		if agent.flat {
			entry.set("command", raw(command))
		} else {
			entry.set("hooks", raw([]hookCommand{{"command", command, 10}}))
		}
		kept = append(kept, raw(entry))
	}
	if !install && !found {
		return data, false, nil
	}
	if len(kept) == 0 {
		hooks.set(agent.event, nil)
	} else {
		hooks.set(agent.event, raw(kept))
	}
	if len(hooks) == 0 {
		root.set("hooks", nil)
	} else {
		root.set("hooks", raw(hooks))
	}
	return formatHooks(root, found)
}

func formatHooks(root object, found bool) ([]byte, bool, error) {
	out, err := indentJSON(root)
	if err != nil {
		return nil, false, err
	}
	return out, found, nil
}

func indentJSON(root object) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, raw(root), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// configDirExists reports whether an agent's config directory is present.
// A path that exists but is not a directory is an error.
func configDirExists(dir string) (bool, error) {
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s: not a directory", dir)
	}
	return true, nil
}

// readJSONOrEmpty reads a config file, or an empty JSON object when the file
// does not exist yet. The mode is the file's own, or 0600 for a new file.
func readJSONOrEmpty(path string) (data []byte, mode os.FileMode, absent bool, err error) {
	data, err = os.ReadFile(path)
	if os.IsNotExist(err) {
		return []byte("{}"), 0600, true, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, false, err
	}
	return data, info.Mode().Perm(), false, nil
}

func Hooks(operation string, output io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if root := config.InstalledReleaseRoot(); root != "" {
		exe = filepath.Join(root, "current", "herdr-mobile-relay")
	}
	refusedDirs := []string{os.TempDir()}
	if cache := os.Getenv("GOCACHE"); cache != "" {
		refusedDirs = append(refusedDirs, cache)
	} else if cache, err := os.UserCacheDir(); err == nil {
		refusedDirs = append(refusedDirs, filepath.Join(cache, "go-build"))
	}
	return manageHooks(home, exe, refusedDirs, operation, output)
}

func manageHooks(home, binary string, refusedDirs []string, operation string, output io.Writer) error {
	if operation == "install" {
		resolvedBinary, err := filepath.EvalSymlinks(binary)
		if err != nil {
			return fmt.Errorf("agent-hooks: %w", err)
		}
		for _, dir := range refusedDirs {
			resolvedDir, err := filepath.EvalSymlinks(dir)
			if err != nil {
				continue
			}
			if resolvedBinary == resolvedDir || strings.HasPrefix(resolvedBinary, resolvedDir+string(filepath.Separator)) {
				return fmt.Errorf("agent-hooks: %s is a temporary go run build; run install from an installed or built herdr-mobile-relay binary", binary)
			}
		}
	}
	type change struct {
		path string
		data []byte
		mode os.FileMode
	}
	var changes []change
	var notes []string
	var feedback bytes.Buffer
	for _, agent := range hookAgents {
		dir := filepath.Join(home, agent.dir)
		path := filepath.Join(dir, agent.file)
		exists, err := configDirExists(dir)
		if err != nil {
			return err
		}
		if !exists {
			fmt.Fprintf(&feedback, "%s: config absent\n", agent.name)
			continue
		}
		data, mode, absent, err := readJSONOrEmpty(path)
		if err != nil {
			return err
		}
		updated, found, err := editHooks(data, agent, binary, operation == "install")
		if err != nil {
			return fmt.Errorf("%s: refusing to edit: %w", path, err)
		}
		state := "not installed"
		if absent {
			state = "config absent"
		} else if found {
			state = "installed"
		}
		switch operation {
		case "install":
			state = "installed"
			if agent.installNote != "" {
				notes = append(notes, agent.installNote)
			}
		case "uninstall":
			if !absent {
				state = "not installed"
			}
		}
		fmt.Fprintf(&feedback, "%s: %s\n", agent.name, state)
		if operation == "status" {
			continue
		}
		if !bytes.Equal(updated, data) {
			changes = append(changes, change{path, updated, mode})
		}
	}
	if err := manageOpenCodePlugin(home, binary, operation, &feedback); err != nil {
		return err
	}
	if err := manageOmpExtension(home, binary, operation, &feedback); err != nil {
		return err
	}
	if operation == "install" {
		fmt.Fprintln(&feedback, "Restart Claude Code, Codex, Cursor, Omp, OpenCode, Qoder and agy to load session hooks; agy reports new conversations only. Hermes, Grok and Kimi need no hook.")
	}
	for _, c := range changes {
		if _, err := atomicfile.Replace(filepath.Dir(c.path), c.path, ".agent-hooks-*", c.data, c.mode); err != nil {
			return fmt.Errorf("%s: %w", c.path, err)
		}
	}
	for _, note := range notes {
		fmt.Fprintln(&feedback, note)
	}
	_, err := io.Copy(output, &feedback)
	return err
}
