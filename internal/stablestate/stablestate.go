package stablestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const Owner = "herdr-mobile-relay-stable-setup-v1"

var UUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func Default(envFile string) map[string]any {
	return map[string]any{
		"owner":                       Owner,
		"schema":                      float64(1),
		"stage":                       "initialized",
		"env_file":                    envFile,
		"tunnel_uuid":                 "",
		"tunnel_name":                 "",
		"hostname":                    "",
		"credentials_path":            "",
		"config_path":                 "",
		"created_tunnel":              false,
		"created_dns":                 false,
		"dns_route_attempted":         false,
		"created_credentials":         false,
		"created_config":              false,
		"service_installed_by_wizard": false,
		"service_preexisting":         nil,
		"env_created_by_wizard":       false,
		"env_config_added_by_wizard":  false,
		"tunnel_deleted":              false,
		"dns_cleanup_required":        false,
	}
}

func Run(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return usage()
	}
	command, args := args[0], args[1:]
	switch command {
	case "init":
		if len(args) < 1 || len(args) > 2 {
			return usage()
		}
		if _, err := os.Stat(args[0]); err == nil {
			_, err = ReadState(args[0])
			return err
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		envFile := ""
		if len(args) == 2 {
			envFile = args[1]
		}
		return Write(args[0], Default(envFile))
	case "get":
		if len(args) != 2 {
			return usage()
		}
		state, err := ReadState(args[0])
		if err != nil {
			return err
		}
		if value, ok := state[args[1]]; ok && value != nil {
			fmt.Fprintln(stdout, printable(value))
		}
		return nil
	case "update":
		if len(args) < 2 {
			return usage()
		}
		state, err := ReadState(args[0])
		if err != nil {
			return err
		}
		for _, assignment := range args[1:] {
			key, value, ok := strings.Cut(assignment, "=")
			if !ok || key == "" {
				return fmt.Errorf("invalid state assignment: %s", assignment)
			}
			if key == "owner" || key == "schema" {
				return fmt.Errorf("state field cannot be changed: %s", key)
			}
			state[key] = parseValue(value)
		}
		return Write(args[0], state)
	case "show":
		if len(args) != 1 {
			return usage()
		}
		state, err := ReadState(args[0])
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(orderedState(state), "", "  ")
		fmt.Fprintln(stdout, string(data))
		return nil
	case "credential-id", "create-id":
		if len(args) != 1 {
			return usage()
		}
		value, err := readJSON(args[0])
		if err != nil {
			return err
		}
		return printUUID(stdout, value, args[0])
	case "tunnel-id-by-name":
		if len(args) != 2 {
			return usage()
		}
		value, err := readJSON(args[0])
		if err != nil {
			return err
		}
		items, err := tunnelList(value)
		if err != nil {
			return err
		}
		var matches []map[string]any
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if ok && entry["name"] == args[1] && !deleted(entry) {
				matches = append(matches, entry)
			}
		}
		if len(matches) == 0 {
			return nil
		}
		if len(matches) != 1 {
			return fmt.Errorf("Cloudflare returned multiple active tunnels named %s", args[1])
		}
		return printUUID(stdout, matches[0], "Cloudflare tunnel named "+args[1])
	case "tunnel-list-has", "tunnel-name-by-id":
		if len(args) != 2 || !UUIDPattern.MatchString(args[1]) {
			return usage()
		}
		value, err := readJSON(args[0])
		if err != nil {
			return err
		}
		items, err := tunnelList(value)
		if err != nil {
			return err
		}
		expected := strings.ToLower(args[1])
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok || deleted(entry) || uuidFrom(entry) != expected {
				continue
			}
			if command == "tunnel-name-by-id" {
				name, _ := entry["name"].(string)
				if name == "" {
					break
				}
				fmt.Fprintln(stdout, name)
			}
			return nil
		}
		return fmt.Errorf("Cloudflare tunnel %s was not found", args[1])
	case "health-valid":
		if len(args) != 1 {
			return usage()
		}
		value, err := readJSON(args[0])
		if err != nil {
			return err
		}
		_, err = validHealth(value, "Relay")
		return err
	case "health-match":
		if len(args) != 2 {
			return usage()
		}
		local, err := readJSON(args[0])
		if err != nil {
			return err
		}
		public, err := readJSON(args[1])
		if err != nil {
			return err
		}
		return HealthMatch(local, public)
	default:
		return usage()
	}
}

func ReadState(filename string) (map[string]any, error) {
	value, err := readJSON(filename)
	if err != nil {
		return nil, err
	}
	state, ok := value.(map[string]any)
	if !ok || state["owner"] != Owner {
		return nil, fmt.Errorf("state file is not owned by Herdr Mobile Relay: %s", filename)
	}
	return state, nil
}

func Write(filename string, state map[string]any) error {
	if state["owner"] != Owner {
		return errors.New("refusing to write unowned stable state")
	}
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(orderedState(state), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(directory, "."+filepath.Base(filename)+".")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filename); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	return err
}

func HealthMatch(localValue, publicValue any) error {
	local, err := validHealth(localValue, "Local")
	if err != nil {
		return err
	}
	public, err := validHealth(publicValue, "Public")
	if err != nil {
		return err
	}
	for _, key := range []string{"instance", "version", "protocol"} {
		if fmt.Sprint(public[key]) != fmt.Sprint(local[key]) {
			return fmt.Errorf("public health %s does not match the local relay (%v != %v)", key, public[key], local[key])
		}
	}
	return nil
}

func validHealth(value any, label string) (map[string]any, error) {
	health, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s health response was not a JSON object", label)
	}
	if health["status"] != "ok" {
		return nil, fmt.Errorf("%s health status is not ok", label)
	}
	if instance, ok := health["instance"].(string); !ok || instance == "" {
		return nil, fmt.Errorf("%s health response has no relay instance ID", label)
	}
	if version, ok := health["version"].(string); !ok || version == "" {
		return nil, fmt.Errorf("%s health response has no relay version", label)
	}
	if _, ok := health["protocol"].(float64); !ok {
		return nil, fmt.Errorf("%s health response has no numeric relay protocol", label)
	}
	return health, nil
}

func readJSON(filename string) (any, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("cannot read valid JSON from %s: %w", filename, err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("cannot read valid JSON from %s: %w", filename, err)
	}
	return value, nil
}

func tunnelList(value any) ([]any, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("Cloudflare tunnel list output was not a JSON list")
	}
	return items, nil
}

func uuidFrom(value any) string {
	mapping, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"TunnelID", "tunnel_id", "id", "ID"} {
		candidate, _ := mapping[key].(string)
		if UUIDPattern.MatchString(candidate) {
			return strings.ToLower(candidate)
		}
	}
	return ""
}

func printUUID(stdout *os.File, value any, source string) error {
	id := uuidFrom(value)
	if id == "" {
		return fmt.Errorf("no tunnel UUID found in %s", source)
	}
	fmt.Fprintln(stdout, id)
	return nil
}

func deleted(value map[string]any) bool {
	deleted, exists := value["deletedAt"]
	return exists && deleted != nil && deleted != ""
}

func parseValue(value string) any {
	switch value {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	default:
		return value
	}
}

func printable(value any) string {
	switch typed := value.(type) {
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return fmt.Sprintf("%g", typed)
	default:
		return fmt.Sprint(typed)
	}
}

// JSON maps are deterministically ordered by encoding/json. Copying also
// prevents callers from mutating state while it is marshaled.
func orderedState(state map[string]any) map[string]any {
	keys := make([]string, 0, len(state))
	for key := range state {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string]any, len(state))
	for _, key := range keys {
		result[key] = state[key]
	}
	return result
}

func usage() error {
	return errors.New("usage: stable-state {init|get|update|show|credential-id|create-id|tunnel-id-by-name|tunnel-list-has|tunnel-name-by-id|health-valid|health-match} ...")
}
