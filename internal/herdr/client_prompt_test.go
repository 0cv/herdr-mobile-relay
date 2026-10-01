package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptDispatchesOnce(t *testing.T) {
	tests := []struct {
		name       string
		diagnostic string
		code       string
		refused    bool
		transient  bool
	}{
		{name: "success"},
		{
			name:       "inactive agent",
			diagnostic: `{"error":{"code":"agent_not_ready","message":"agent pane-1 is not an active named agent"}}`,
			code:       "agent_not_ready",
			refused:    true,
		},
		{
			name:       "agent no longer foreground",
			diagnostic: `{"error":{"code":"agent_not_ready","message":"agent pane-1 is no longer the pane foreground process"}}`,
			code:       "agent_not_ready",
			refused:    true,
		},
		{
			name:       "not ready without diagnostic message",
			diagnostic: `{"error":{"code":"agent_not_ready"}}`,
			code:       "agent_not_ready",
			refused:    true,
		},
		{
			name:       "server not running",
			diagnostic: `{"error":{"code":"server_not_running","message":"no server"}}`,
			code:       "server_not_running",
			refused:    true,
			transient:  true,
		},
		{
			name:       "protocol mismatch",
			diagnostic: `{"error":{"code":"protocol_mismatch","message":"incompatible"}}`,
			code:       "protocol_mismatch",
			refused:    true,
		},
		{
			name:       "uncertain submission",
			diagnostic: `{"error":{"code":"agent_prompt_failed","message":"pty actor closed"}}`,
			code:       "agent_prompt_failed",
		},
		{
			name:       "unstructured transport failure",
			diagnostic: "connection reset before Herdr acknowledged the prompt",
		},
		{
			name:       "unstructured not ready diagnostic",
			diagnostic: "agent_not_ready: agent pane-1 is not an active named agent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			argsPath := filepath.Join(dir, "args")
			bin := filepath.Join(dir, "herdr")
			script := "#!/bin/sh\n" +
				"printf '%s\\n' \"$@\" >> \"$HERDR_TEST_ARGS\"\n" +
				"if [ -n \"$HERDR_TEST_ERROR\" ]; then\n" +
				"  printf '%s' \"$HERDR_TEST_ERROR\" >&2\n" +
				"  exit 1\n" +
				"fi\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake Herdr: %v", err)
			}
			t.Setenv("HERDR_TEST_ARGS", argsPath)
			t.Setenv("HERDR_TEST_ERROR", test.diagnostic)
			client := NewClient(bin, filepath.Join(dir, "herdr.sock"))
			t.Cleanup(func() { _ = client.Close() })

			text := "Review this command without executing it:\nprintf 'hello world'"
			err := client.Prompt(context.Background(), "pane-1", text)
			wantError := test.diagnostic != ""
			if (err != nil) != wantError {
				t.Fatalf("Prompt() error = %v, want error = %v", err, wantError)
			}
			if IsRefused(err) != test.refused || IsTransientRefused(err) != test.transient {
				t.Fatalf("error = %v, refused = %v, transient = %v; want %v, %v", err, IsRefused(err), IsTransientRefused(err), test.refused, test.transient)
			}
			if code := RefusalCode(err); code != test.code {
				t.Fatalf("RefusalCode() = %q, want %q", code, test.code)
			}
			if errors.Is(err, ErrDispatchedUnknown) != wantError || errors.Is(err, ErrNotStarted) {
				t.Fatalf("error = %v, want original subprocess dispatch boundary", err)
			}
			if test.code != "" {
				var cliErr *CLIError
				if !errors.As(err, &cliErr) || cliErr.Code != test.code {
					t.Fatalf("error = %v, want CLIError with code %q", err, test.code)
				}
			}
			data, readErr := os.ReadFile(argsPath)
			if readErr != nil {
				t.Fatalf("read Herdr arguments: %v", readErr)
			}
			want := strings.Join([]string{"agent", "prompt", "pane-1", text, ""}, "\n")
			if string(data) != want {
				t.Fatalf("Herdr arguments = %q, want exactly one prompt invocation %q", data, want)
			}
		})
	}
}

func TestPromptCannotStart(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "missing executable"
		if canceled {
			name = "canceled context"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			client := NewClient(filepath.Join(dir, "missing-herdr"), filepath.Join(dir, "herdr.sock"))
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantCause := os.ErrNotExist
			if canceled {
				cancel()
				wantCause = context.Canceled
			}

			err := client.Prompt(ctx, "pane-1", "hello")
			if !errors.Is(err, ErrNotStarted) || errors.Is(err, ErrDispatchedUnknown) || !errors.Is(err, wantCause) {
				t.Fatalf("error = %v, want not started with cause %v", err, wantCause)
			}
			if IsRefused(err) || RefusalCode(err) != "" {
				t.Fatalf("error = %v, want no CLI refusal", err)
			}
		})
	}
}

func TestAgentNotReadyIsNotAGlobalRefusal(t *testing.T) {
	err := &CLIError{Code: "agent_not_ready", Message: "agent is blocked during startup"}
	for _, err := range []error{err, &OutcomeError{Started: true, Err: err}} {
		if IsRefused(err) || IsTransientRefused(err) {
			t.Fatalf("error = %v, want operation-specific classification", err)
		}
		if code := RefusalCode(err); code != "agent_not_ready" {
			t.Fatalf("RefusalCode() = %q, want original code", code)
		}
	}
}
