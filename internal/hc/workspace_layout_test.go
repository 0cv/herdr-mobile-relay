package hc

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const spaceFixture = `{"unknown":9007199254740993,"spaces":[{"id":"a","name":"A","folder":"/a","extra":1.234567890123456789,"tabs":[{"id":"t1","root":{"future":true}},{"id":"t2","name":"two"}]},{"id":"b","name":"B","tabs":[{"id":"t3","future":[1,2]}]},{"id":"c","name":"C","tabs":[]}]}`

func TestWorkspaceLayoutEdits(t *testing.T) {
	for _, tc := range []struct {
		name, order string
		run         func(*Client) error
	}{
		{"rename", "abc", func(c *Client) error { return c.WorkspaceRename(t.Context(), "a", "new") }},
		{"start", "abc", func(c *Client) error { return c.WorkspaceMove(t.Context(), "a", 0) }},
		{"adjacent", "abc", func(c *Client) error { return c.WorkspaceMove(t.Context(), "a", 1) }},
		{"middle", "bac", func(c *Client) error { return c.WorkspaceMove(t.Context(), "a", 2) }},
		{"end", "bca", func(c *Client) error { return c.WorkspaceMove(t.Context(), "a", 3) }},
		{"backward", "cab", func(c *Client) error { return c.WorkspaceMove(t.Context(), "c", 0) }},
		{"block end", "cab", func(c *Client) error { return c.WorkspaceMoveBlock(t.Context(), []string{"a", "b"}, "") }},
		{"block before", "bca", func(c *Client) error { return c.WorkspaceMoveBlock(t.Context(), []string{"b", "c"}, "a") }},
		{"block noncontiguous", "acb", func(c *Client) error { return c.WorkspaceMoveBlock(t.Context(), []string{"c", "a"}, "b") }},
		{"block all", "abc", func(c *Client) error { return c.WorkspaceMoveBlock(t.Context(), []string{"c", "b", "a"}, "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got json.RawMessage
			var reads, writes atomic.Int32
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method == "doc.get" {
					reads.Add(1)
					reply(conn, req.ID, "result", map[string]any{"value": json.RawMessage(spaceFixture), "version": 7})
					return
				}
				writes.Add(1)
				var wire struct {
					Params struct {
						Value   json.RawMessage
						Version uint64
					}
				}
				if err := json.Unmarshal(req.Raw, &wire); err != nil {
					t.Error(err)
				}
				if req.Method != "doc.set" || wire.Params.Version != 7 {
					t.Errorf("request: %s", req.Raw)
				}
				got = wire.Params.Value
				reply(conn, req.ID, "result", map[string]any{"version": 8})
			})
			if err := tc.run(New(socket)); err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 1 || writes.Load() != 1 {
				t.Fatal("not a single read-CAS edit")
			}
			decode := func(raw string) map[string]any {
				var result map[string]any
				decoder := json.NewDecoder(strings.NewReader(raw))
				decoder.UseNumber()
				if err := decoder.Decode(&result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			want := decode(spaceFixture)
			spaces := want["spaces"].([]any)
			ordered := []any{}
			for _, id := range tc.order {
				for _, space := range spaces {
					if space.(map[string]any)["id"] == string(id) {
						ordered = append(ordered, space)
					}
				}
			}
			want["spaces"] = ordered
			if tc.name == "rename" {
				spaces[0].(map[string]any)["name"] = "new"
			}
			if !reflect.DeepEqual(decode(string(got)), want) {
				t.Fatalf("content changed: %s; want %+v", got, want)
			}
		})
	}
}

func TestWorkspaceLayoutRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		run       func(context.Context, *Client) error
	}{
		{"synthetic", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceRename(ctx, "core", "new") }},
		{"remote", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMove(ctx, "host/a", 0) }},
		{"missing", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceRename(ctx, "missing", "new") }},
		{"duplicate spaces", strings.Replace(spaceFixture, `"id":"b"`, `"id":"a"`, 1), func(ctx context.Context, c *Client) error { return c.WorkspaceRename(ctx, "a", "new") }},
		{"duplicate tabs", strings.Replace(spaceFixture, `"id":"t2"`, `"id":"t1"`, 1), func(ctx context.Context, c *Client) error { return c.WorkspaceRename(ctx, "a", "new") }},
		{"negative", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMove(ctx, "a", -1) }},
		{"past end", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMove(ctx, "a", 4) }},
		{"empty block", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, nil, "") }},
		{"duplicate selection", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"a", "a"}, "") }},
		{"missing selection", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"missing"}, "") }},
		{"synthetic selection", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"core"}, "") }},
		{"remote destination", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"a"}, "host/b") }},
		{"synthetic destination", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"a"}, "core") }},
		{"missing destination", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"a"}, "missing") }},
		{"selected destination", spaceFixture, func(ctx context.Context, c *Client) error { return c.WorkspaceMoveBlock(ctx, []string{"a"}, "a") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method != "doc.get" {
					t.Error("unexpected write")
				}
				reply(conn, req.ID, "result", map[string]any{"value": json.RawMessage(tc.doc), "version": 1})
			})
			if err := tc.run(t.Context(), New(socket)); !herdr.IsRefused(err) {
				t.Fatalf("refusal: %v", err)
			}
		})
	}
}

func TestWorkspaceLayoutConflict(t *testing.T) {
	for _, operation := range []string{"rename", "move", "block"} {
		t.Run(operation, func(t *testing.T) {
			var reads, writes atomic.Int32
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method == "doc.get" {
					reads.Add(1)
					reply(conn, req.ID, "result", map[string]any{"value": json.RawMessage(spaceFixture), "version": 1})
					return
				}
				writes.Add(1)
				reply(conn, req.ID, "error", map[string]any{"code": -32000, "data": map[string]any{"kind": "conflict"}})
			})
			c := New(socket)
			var err error
			switch operation {
			case "rename":
				err = c.WorkspaceRename(t.Context(), "a", "new")
			case "move":
				err = c.WorkspaceMove(t.Context(), "a", 3)
			case "block":
				err = c.WorkspaceMoveBlock(t.Context(), []string{"a", "b"}, "")
			}
			if herdr.RefusalCode(err) != "layout_conflict" || reads.Load() != 1 || writes.Load() != 1 {
				t.Fatalf("conflict retried or lost: %v, %d/%d", err, reads.Load(), writes.Load())
			}
		})
	}
}
