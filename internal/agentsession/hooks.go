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

func editHooks(data []byte, agent, binary string, install bool) ([]byte, bool, error) {
	root, err := parseObject(data)
	if err != nil {
		return nil, false, err
	}
	var hooks object
	if value := root.get("hooks"); value != nil {
		hooks, err = parseObject(value)
		if err != nil {
			return nil, false, fmt.Errorf("hooks: %w", err)
		}
	}
	var entries []json.RawMessage
	if value := hooks.get("SessionStart"); value != nil {
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, false, err
		}
	}
	found := false
	kept := make([]json.RawMessage, 0, len(entries)+1)
	for _, entry := range entries {
		obj, err := parseObject(entry)
		if err != nil {
			return nil, false, fmt.Errorf("SessionStart entry: %w", err)
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
			if hook.Type == "command" && strings.HasSuffix(strings.TrimSpace(hook.Command), " agent-session "+agent) {
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
		command := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "' agent-session " + agent
		entry := object{}
		if agent == "claude" {
			entry.set("matcher", raw("^(startup|resume|clear|compact|fork)$"))
		}
		entry.set("hooks", raw([]hookCommand{{"command", command, 10}}))
		kept = append(kept, raw(entry))
	}
	if !install && !found {
		return data, false, nil
	}
	if len(kept) == 0 {
		hooks.set("SessionStart", nil)
	} else {
		hooks.set("SessionStart", raw(kept))
	}
	if len(hooks) == 0 {
		root.set("hooks", nil)
	} else {
		root.set("hooks", raw(hooks))
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw(root), "", "  "); err != nil {
		return nil, false, err
	}
	out.WriteByte('\n')
	return out.Bytes(), found, nil
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
	return manageHooks(home, exe, os.TempDir(), operation, output)
}

func manageHooks(home, binary, tempDir, operation string, output io.Writer) error {
	if operation == "install" {
		resolvedBinary, err := filepath.EvalSymlinks(binary)
		if err != nil {
			return fmt.Errorf("agent-hooks: %w", err)
		}
		resolvedTemp, err := filepath.EvalSymlinks(tempDir)
		if err != nil {
			return fmt.Errorf("agent-hooks: %w", err)
		}
		if resolvedBinary == resolvedTemp || strings.HasPrefix(resolvedBinary, resolvedTemp+string(filepath.Separator)) {
			return fmt.Errorf("agent-hooks: %s is a temporary build; run install from an installed or built herdr-mobile-relay binary", binary)
		}
	}
	codexInstalled := false
	type change struct {
		path string
		data []byte
		mode os.FileMode
	}
	var changes []change
	var feedback bytes.Buffer
	for _, agent := range []string{"claude", "codex"} {
		dir := filepath.Join(home, "."+agent)
		name := "hooks.json"
		if agent == "claude" {
			name = "settings.json"
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			fmt.Fprintf(&feedback, "%s: config absent\n", agent)
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s: not a directory", dir)
		}
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
			if agent == "codex" {
				codexInstalled = true
			}
		case "uninstall":
			if !absent {
				state = "not installed"
			}
		}
		fmt.Fprintf(&feedback, "%s: %s\n", agent, state)
		if operation == "status" {
			continue
		}
		if !bytes.Equal(updated, data) {
			changes = append(changes, change{path, updated, mode})
		}
	}
	for _, c := range changes {
		if _, err := atomicfile.Replace(filepath.Dir(c.path), c.path, ".agent-hooks-*", c.data, c.mode); err != nil {
			return fmt.Errorf("%s: %w", c.path, err)
		}
	}
	if codexInstalled {
		fmt.Fprintln(&feedback, "codex: start Codex with --no-daemon in herdr.app terminals so its sessions can be matched to their terminal")
	}
	_, err := io.Copy(output, &feedback)
	return err
}
