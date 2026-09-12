package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandSubprocess(t *testing.T) {
	if os.Getenv("HERDR_TEST_COMMAND_HELPER") != "1" {
		return
	}

	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("HERDR_TEST_COMMAND_ARGS")), &args); err != nil {
		panic(err)
	}
	os.Args = append([]string{os.Args[0]}, args...)
	main()
	os.Exit(0)
}

func TestCommandBoundaries(t *testing.T) {
	t.Run("serve usage", func(t *testing.T) {
		t.Setenv("HERDR_RELAY_LOG_FORMAT", "json")
		t.Setenv("HERDR_RELAY_LOG_LEVEL", "info")
		t.Setenv("JOURNAL_STREAM", "")

		result := runCommandSubprocess(t, "serve", "bad")
		if result.exitCode != 2 {
			t.Fatalf("exit code = %d, want 2; stderr = %q", result.exitCode, result.stderr)
		}
		if len(result.stdout) != 0 {
			t.Fatalf("stdout = %q, want empty", result.stdout)
		}
		record := decodeSingleJSONRecord(t, result.stderr)
		if record["level"] != "ERROR" || record["msg"] != "relay failed" {
			t.Fatalf("record = %#v, want one relay failure ERROR", record)
		}
		message, ok := record["error"].(string)
		if !ok || !strings.Contains(message, "serve does not accept arguments") {
			t.Fatalf("record = %#v, want usage error", record)
		}
	})

	t.Run("invalid config startup", func(t *testing.T) {
		t.Setenv("HERDR_RELAY_LOG_FORMAT", "json")
		t.Setenv("HERDR_RELAY_LOG_LEVEL", "verbose")
		t.Setenv("JOURNAL_STREAM", "")

		result := runCommandSubprocess(t)
		if result.exitCode != 1 {
			t.Fatalf("exit code = %d, want 1; stderr = %q", result.exitCode, result.stderr)
		}
		if len(result.stdout) != 0 {
			t.Fatalf("stdout = %q, want empty", result.stdout)
		}
		record := decodeSingleJSONRecord(t, result.stderr)
		if record["level"] != "ERROR" || record["msg"] != "relay failed" {
			t.Fatalf("record = %#v, want one relay failure ERROR", record)
		}
		message, ok := record["error"].(string)
		if !ok || !strings.Contains(message, "invalid HERDR_RELAY_LOG_LEVEL") {
			t.Fatalf("record = %#v, want invalid log-level error", record)
		}
	})

	t.Run("version json", func(t *testing.T) {
		t.Setenv("HERDR_RELAY_LOG_LEVEL", "verbose")
		t.Setenv("JOURNAL_STREAM", "")

		result := runCommandSubprocess(t, "version", "--json")
		if result.exitCode != 0 {
			t.Fatalf("exit code = %d, want 0; stderr = %q", result.exitCode, result.stderr)
		}
		if len(result.stderr) != 0 {
			t.Fatalf("stderr = %q, want empty", result.stderr)
		}
		output := bytes.TrimSpace(result.stdout)
		if len(output) == 0 || output[0] == '<' {
			t.Fatalf("stdout = %q, want unprefixed JSON", result.stdout)
		}
		var record map[string]string
		if err := json.Unmarshal(output, &record); err != nil {
			t.Fatalf("stdout = %q, want directly parseable JSON: %v", result.stdout, err)
		}
		for _, key := range []string{"version", "revision", "target"} {
			if record[key] == "" {
				t.Errorf("version JSON field %q is empty: %#v", key, record)
			}
		}
	})
}

type commandResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

func runCommandSubprocess(t *testing.T, args ...string) commandResult {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, directory := range []string{"home", "config", "cache", "data", "runtime", "tmp", "plugin", "web", "releases"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	herdr := filepath.Join(root, "herdr")
	if err := os.WriteFile(herdr, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandSubprocess$")
	command.Env = commandEnvironment(string(encoded), root, herdr)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	result := commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if ctx.Err() != nil {
		t.Fatalf("command timed out: stdout = %q, stderr = %q", result.stdout, result.stderr)
	}
	if err == nil {
		return result
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("run command: %v", err)
	}
	result.exitCode = exitError.ExitCode()
	return result
}

func commandEnvironment(args, root, herdr string) []string {
	return []string{
		"HOME=" + filepath.Join(root, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "runtime"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"PATH=/usr/bin:/bin",
		"LANG=C",
		"LC_ALL=C",
		"HERDR_RELAY_HOST=127.0.0.1",
		"HERDR_RELAY_PORT=1",
		"HERDR_RELAY_PLUGIN_PORT=2",
		"HERDR_RELAY_TOKEN=",
		"HERDR_RELAY_INSTANCE_ID=",
		"HERDR_RELAY_ENV=" + filepath.Join(root, "config", "relay.env"),
		"HERDR_PLUGIN_CONFIG_DIR=" + filepath.Join(root, "plugin"),
		"HERDR_WEB_ROOT=" + filepath.Join(root, "web"),
		"HERDR_BIN=" + herdr,
		"HERDR_SOCKET_PATH=" + filepath.Join(root, "runtime", "relay.sock"),
		"HERDR_RELAY_POLL_INTERVAL=2",
		"HERDR_RELAY_LOG_FORMAT=" + os.Getenv("HERDR_RELAY_LOG_FORMAT"),
		"HERDR_RELAY_LOG_LEVEL=" + os.Getenv("HERDR_RELAY_LOG_LEVEL"),
		"HERDR_RELAY_SERVICE_NAME=herdr-command-test.service",
		"HERDR_ALLOWED_ORIGINS=",
		"HERDR_GATEWAY_URL=",
		"HERDR_GATEWAY_SELECTION=",
		"HERDR_WEBRTC_UDP_PORT=0",
		"HERDR_TRANSPORT_FORCE_RELAY=false",
		"HERDR_REACHABILITY_PORT_MAPPING=false",
		"HERDR_RELAY_REARM_BOOTSTRAP=false",
		"HERDR_RELEASE_ROOT=" + filepath.Join(root, "releases"),
		"JOURNAL_STREAM=",
		"HERDR_TEST_COMMAND_HELPER=1",
		"HERDR_TEST_COMMAND_ARGS=" + args,
	}
}

func decodeSingleJSONRecord(t *testing.T, data []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var record map[string]any
	if err := decoder.Decode(&record); err != nil {
		t.Fatalf("JSON output = %q: %v", data, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON output = %q, want exactly one record; second decode = %v", data, err)
	}
	return record
}
