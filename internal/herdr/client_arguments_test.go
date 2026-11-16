package herdr

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCLIArguments(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		reply string
		call  func(client *Client) error
		want  []string
	}{
		{
			name: "WorkspaceCreate",
			call: func(c *Client) error { _, err := c.WorkspaceCreate(ctx, "/work/project", "My Project"); return err },
			want: []string{"workspace", "create", "--cwd", "/work/project", "--label", "My Project", "--no-focus"},
		},
		{
			name: "WorkspaceRename",
			call: func(c *Client) error { return c.WorkspaceRename(ctx, "w1", "New Label") },
			want: []string{"workspace", "rename", "w1", "New Label"},
		},
		{
			name: "TabCreate",
			call: func(c *Client) error { _, err := c.TabCreate(ctx, "w1", "/work/project", "My Tab"); return err },
			want: []string{"tab", "create", "--workspace", "w1", "--cwd", "/work/project", "--label", "My Tab", "--no-focus"},
		},
		{
			name: "TabRename",
			call: func(c *Client) error { return c.TabRename(ctx, "w1:t1", "New Tab") },
			want: []string{"tab", "rename", "w1:t1", "New Tab"},
		},
		{
			name: "PaneRun",
			call: func(c *Client) error { return c.PaneRun(ctx, "w1:p1", []string{"codex", "--model", "a b"}) },
			want: []string{"pane", "run", "w1:p1", "codex --model 'a b'"},
		},
		{
			name:  "AgentGet",
			reply: `{"result":{"pane_id":"w1:p1","agent":"codex"}}`,
			call:  func(c *Client) error { _, err := c.AgentGet(ctx, "w1:p1"); return err },
			want:  []string{"agent", "get", "w1:p1"},
		},
		{
			name: "SendText",
			call: func(c *Client) error { return c.SendText(ctx, "w1:p1", "two words\n") },
			want: []string{"pane", "send-text", "w1:p1", "two words\n"},
		},
		{
			name: "StopPane",
			call: func(c *Client) error { return c.StopPane(ctx, "w1:p1") },
			want: []string{"pane", "close", "w1:p1"},
		},
		{
			name: "RenameAgent",
			call: func(c *Client) error { return c.RenameAgent(ctx, "w1:p1", "Agent One") },
			want: []string{"agent", "rename", "w1:p1", "Agent One"},
		},
		{
			name: "StartAgent",
			call: func(c *Client) error {
				_, err := c.StartAgent(ctx, "project-codex", "codex", "w1:p1", 12_345)
				return err
			},
			want: []string{"agent", "start", "project-codex", "--kind", "codex", "--pane", "w1:p1", "--timeout", "12345"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "args")
			bin := filepath.Join(dir, "herdr")
			reply := cmp.Or(test.reply, `{"result":{"agent":{"pane_id":"w1:p1"}}}`)
			script := "#!/bin/sh\n" +
				"printf '%s\\0' \"$@\" > \"" + record + "\"\n" +
				"printf '%s\\n' '" + reply + "'\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatalf("write Herdr script: %v", err)
			}
			client := NewClient(bin, filepath.Join(dir, "herdr.sock"))

			if err := test.call(client); err != nil {
				t.Fatalf("%s() error = %v", test.name, err)
			}
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("read arguments: %v", err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("arguments = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCommandDeadlineAfterStartIsDispatchedUnknown(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := NewClient(bin, filepath.Join(dir, "herdr.sock"))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := client.SendText(ctx, "w1:p1", "hello")
	if !errors.Is(err, ErrDispatchedUnknown) || errors.Is(err, ErrNotStarted) {
		t.Fatalf("error = %v, want dispatched-unknown and not not-started", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the deadline as cause", err)
	}
}

func TestCommandDeadlineRefusedBeforeStartIsNotStarted(t *testing.T) {
	refused := errors.New("target changed")
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	tests := []struct {
		name  string
		ctx   context.Context
		cause error
	}{
		{
			name:  "dispatch check refuses",
			ctx:   WithDispatchCheck(context.Background(), func() error { return refused }),
			cause: refused,
		},
		{name: "deadline already passed", ctx: expired, cause: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "ran")
			bin := filepath.Join(dir, "herdr")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			client := NewClient(bin, filepath.Join(dir, "herdr.sock"))

			err := client.SendText(test.ctx, "w1:p1", "hello")
			if !errors.Is(err, ErrNotStarted) || errors.Is(err, ErrDispatchedUnknown) {
				t.Fatalf("error = %v, want not-started and not dispatched-unknown", err)
			}
			if !errors.Is(err, test.cause) {
				t.Fatalf("error = %v, want cause %v", err, test.cause)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("command ran: %v", statErr)
			}
		})
	}
}
