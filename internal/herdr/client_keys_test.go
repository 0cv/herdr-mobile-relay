package herdr

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSendKeysEncodesTerminalChords(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want []string
	}{
		{"Ctrl letter", []string{"Ctrl+C"}, []string{"pane", "send-keys", "pane-1", "ctrl+c"}},
		{"Shift Tab", []string{"Shift+Tab"}, []string{"pane", "send-text", "pane-1", shiftTabSequence}},
		{"PageUp", []string{"PageUp"}, []string{"pane", "send-text", "pane-1", "\x1b[5~"}},
		{"PageDown", []string{"PageDown"}, []string{"pane", "send-text", "pane-1", "\x1b[6~"}},
		{"Home", []string{"Home"}, []string{"pane", "send-text", "pane-1", "\x1b[H"}},
		{"End", []string{"End"}, []string{"pane", "send-text", "pane-1", "\x1b[F"}},
		{"Shift PageUp", []string{"shift+pageup"}, []string{"pane", "send-text", "pane-1", "\x1b[5;2~"}},
		{"Alt PageDown", []string{"alt+pagedown"}, []string{"pane", "send-text", "pane-1", "\x1b[6;3~"}},
		{"Ctrl End", []string{"ctrl+End"}, []string{"pane", "send-text", "pane-1", "\x1b[1;5F"}},
		{"Ctrl Alt Shift Home", []string{"ctrl+alt+shift+home"}, []string{"pane", "send-text", "pane-1", "\x1b[1;8H"}},
		{"Invalid modifier", []string{"meta+home"}, []string{"pane", "send-keys", "pane-1", "meta+home"}},
		{"Duplicate modifier", []string{"ctrl+ctrl+end"}, []string{"pane", "send-keys", "pane-1", "ctrl+ctrl+end"}},
		{"Multiple keys", []string{"PageUp", "Enter"}, []string{"pane", "send-keys", "pane-1", "PageUp", "Enter"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			argsPath := filepath.Join(dir, "args")
			bin := filepath.Join(dir, "herdr")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HERDR_TEST_ARGS\"\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake Herdr: %v", err)
			}
			t.Setenv("HERDR_TEST_ARGS", argsPath)

			client := NewClient(bin, filepath.Join(dir, "herdr.sock"))
			if err := client.SendKeys(context.Background(), "pane-1", tt.keys); err != nil {
				t.Fatalf("SendKeys() error = %v", err)
			}
			data, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatalf("read fake Herdr arguments: %v", err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Herdr arguments = %#v, want %#v", got, tt.want)
			}
		})
	}
}
