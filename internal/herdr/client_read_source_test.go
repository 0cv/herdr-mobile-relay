package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type readPaneCall struct {
	name         string
	read         func(client *Client) (PaneRead, error)
	socketSource string
	cliSource    string
}

var readPaneCalls = []readPaneCall{
	{
		name: "ReadPane",
		read: func(c *Client) (PaneRead, error) {
			return c.ReadPane(context.Background(), "w1:p1", 40, "text")
		},
		socketSource: "recent_unwrapped",
		cliSource:    "recent-unwrapped",
	},
	{
		name: "ReadPaneRecent",
		read: func(c *Client) (PaneRead, error) {
			return c.ReadPaneRecent(context.Background(), "w1:p1", 40, "text")
		},
		socketSource: "recent",
		cliSource:    "recent",
	},
	{
		name: "ReadPaneVisible",
		read: func(c *Client) (PaneRead, error) {
			return c.ReadPaneVisible(context.Background(), "w1:p1", 40, "text")
		},
		socketSource: "visible",
		cliSource:    "visible",
	},
}

func TestReadPaneSendsSourceOverSocket(t *testing.T) {
	for _, test := range readPaneCalls {
		t.Run(test.name, func(t *testing.T) {
			socketPath, requests := startRecordingTestSocket(t, map[string]any{
				"type": "pane_read",
				"read": map[string]any{"text": "content"},
			})
			client := NewClient("/binary/must-not-run", socketPath)

			read, err := test.read(client)
			if err != nil {
				t.Fatalf("%s() error = %v", test.name, err)
			}
			if string(read.Content) != "content" {
				t.Fatalf("content = %q, want %q", read.Content, "content")
			}
			request := <-requests
			if request.Method != "pane.read" || request.Params["source"] != test.socketSource {
				t.Fatalf("request = %+v, want pane.read with source %q", request, test.socketSource)
			}
		})
	}
}

func TestReadPaneCommandFallbackPassesSource(t *testing.T) {
	for _, test := range readPaneCalls {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "args")
			bin := filepath.Join(dir, "herdr")
			script := "#!/bin/sh\n" +
				"printf '%s\\n' \"$*\" > \"" + record + "\"\n" +
				"printf 'content'\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			client := NewClient(bin, filepath.Join(dir, "missing.sock"))

			read, err := test.read(client)
			if err != nil {
				t.Fatalf("%s() error = %v", test.name, err)
			}
			if string(read.Content) != "content" {
				t.Fatalf("content = %q, want %q", read.Content, "content")
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			want := "pane read w1:p1 --lines 40 --source " + test.cliSource + " --format text"
			if got := strings.TrimSpace(string(data)); got != want {
				t.Fatalf("arguments = %q, want %q", got, want)
			}
		})
	}
}
