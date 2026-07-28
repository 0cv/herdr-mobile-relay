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
		{
			name: "Ctrl letter",
			keys: []string{"Ctrl+C"},
			want: []string{"pane", "send-keys", "pane-1", "ctrl+c"},
		},
		{
			name: "Shift Tab",
			keys: []string{"Shift+Tab"},
			want: []string{"pane", "send-text", "pane-1", shiftTabSequence},
		},
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
