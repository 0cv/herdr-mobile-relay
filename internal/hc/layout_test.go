package hc

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const layoutFixture = `{"unknown":9007199254740993,"spaces":[{"id":"s","extra":1.234567890123456789,"tabs":[{"id":"a","name":"A","root":{"type":"stack","active":2,"children":[],"size":0.1234567890123456789},"future":true},{"id":"b","name":"B","root":{}},{"id":"c","name":"C","root":{}}]}]}`

func TestLayoutEdits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		move   bool
		index  int
		want   string
	}{
		{name: "rename", want: strings.Replace(layoutFixture, `"name":"A"`, `"name":"new"`, 1)},
		{name: "start", move: true, index: 0, want: "abc"},
		{name: "adjacent slot", move: true, index: 1, want: "abc"},
		{name: "middle", move: true, index: 2, want: "bac"},
		{name: "end", move: true, index: 3, want: "bca"},
		{name: "last to first", target: "c", move: true, index: 0, want: "cab"},
		{name: "last to middle", target: "c", move: true, index: 1, want: "acb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got json.RawMessage
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method == "doc.get" {
					reply(conn, req.ID, "result", map[string]any{"value": json.RawMessage(layoutFixture), "version": uint64(18446744073709551614)})
					return
				}
				if req.Method != "doc.set" || !strings.Contains(string(req.Raw), `"version":18446744073709551614`) {
					t.Errorf("request: %+v", req)
				}
				var wire struct {
					Params struct{ Value json.RawMessage }
				}
				json.Unmarshal(req.Raw, &wire)
				got = wire.Params.Value
				reply(conn, req.ID, "result", map[string]any{"version": 8})
			})
			client := New(socket)
			var err error
			if tc.move {
				target := tc.target
				if target == "" {
					target = "a"
				}
				err = client.TabMove(t.Context(), target, tc.index)
			} else {
				err = client.TabRename(t.Context(), "a", "new")
			}
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			decoder := json.NewDecoder(strings.NewReader(string(got)))
			decoder.UseNumber()
			if err := decoder.Decode(&actual); err != nil {
				t.Fatal(err)
			}
			if !tc.move {
				decoder = json.NewDecoder(strings.NewReader(tc.want))
				decoder.UseNumber()
				if err := decoder.Decode(&expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("got %s", got)
				}
				return
			}
			tabs := actual.(map[string]any)["spaces"].([]any)[0].(map[string]any)["tabs"].([]any)
			order := ""
			for _, tab := range tabs {
				order += tab.(map[string]any)["id"].(string)
			}
			if order != tc.want {
				t.Fatalf("order %s", order)
			}
			decoder = json.NewDecoder(strings.NewReader(layoutFixture))
			decoder.UseNumber()
			if err := decoder.Decode(&expected); err != nil {
				t.Fatal(err)
			}
			space := expected.(map[string]any)["spaces"].([]any)[0].(map[string]any)
			originals := space["tabs"].([]any)
			reordered := make([]any, 0, len(originals))
			for _, id := range tc.want {
				for _, tab := range originals {
					if tab.(map[string]any)["id"] == string(id) {
						reordered = append(reordered, tab)
					}
				}
			}
			space["tabs"] = reordered
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("reorder changed content: %s", got)
			}
		})
	}
}

func TestLayoutRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, doc, id string
		index         int
	}{
		{"null", "null", "a", 0}, {"missing", "{}", "a", 0},
		{"fallback tab", layoutFixture, "core", 0}, {"label only", layoutFixture, "label", 0},
		{"remote", layoutFixture, "host/a", 0}, {"empty", layoutFixture, "", 0},
		{"fallback space", strings.Replace(layoutFixture, `"id":"s"`, `"id":"core"`, 1), "a", 0},
		{"duplicate tab", strings.Replace(layoutFixture, `"id":"b"`, `"id":"a"`, 1), "a", 0},
		{"missing tab ID", strings.Replace(layoutFixture, `"id":"b"`, `"id":""`, 1), "a", 0},
		{"duplicate space", `{"spaces":[{"id":"s","tabs":[{"id":"a"}]},{"id":"s","tabs":[]}]}`, "a", 0},
		{"negative", layoutFixture, "a", -1}, {"past end", layoutFixture, "a", 4},
		{"size", strings.Replace(layoutFixture, `"A"`, `"`+strings.Repeat("x", 1<<20)+`"`, 1), "a", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method != "doc.get" {
					t.Error("unexpected write")
				}
				reply(conn, req.ID, "result", map[string]any{"value": json.RawMessage(tc.doc), "version": 1})
			})
			if err := New(socket).TabMove(t.Context(), tc.id, tc.index); !herdr.IsRefused(err) {
				t.Fatalf("refusal: %v", err)
			}
		})
	}
}

func TestLayoutCASBoundary(t *testing.T) {
	for _, kind := range []string{"conflict", "disconnect", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var reads, writes atomic.Int32
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method == "doc.get" {
					reads.Add(1)
					reply(conn, req.ID, "result", map[string]any{"value": json.RawMessage(layoutFixture), "version": 1})
					return
				}
				writes.Add(1)
				if kind == "conflict" {
					reply(conn, req.ID, "error", map[string]any{"code": -32000, "data": map[string]any{"kind": "conflict"}})
				}
			})
			ctx := t.Context()
			if kind == "cancel" {
				checks := 0
				ctx = herdr.WithDispatchCheck(ctx, func() error {
					checks++
					if checks == 2 {
						return errors.New("cancelled")
					}
					return nil
				})
			}
			err := New(socket).TabRename(ctx, "a", "new")
			if reads.Load() != 1 {
				t.Fatal("read retried")
			}
			if kind == "cancel" {
				if writes.Load() != 0 || !errors.Is(err, herdr.ErrNotStarted) {
					t.Fatal(err)
				}
				return
			}
			if writes.Load() != 1 {
				t.Fatal("write retried")
			}
			if kind == "conflict" {
				if !herdr.IsRefused(err) || herdr.RefusalCode(err) != "layout_conflict" || errors.Is(err, herdr.ErrDispatchedUnknown) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, herdr.ErrDispatchedUnknown) {
				t.Fatal(err)
			}
		})
	}
}

func TestLayoutMalformedRead(t *testing.T) {
	for _, result := range []string{`{}`, `{"value":{},"version":-1}`, `{"value":{},"version":"1"}`, `{"value":[],"version":1}`} {
		t.Run(result, func(t *testing.T) {
			socket := fakeCore(t, 1, func(conn net.Conn, req fakeRequest) {
				if req.Method != "doc.get" {
					t.Error("unexpected write")
				}
				reply(conn, req.ID, "result", json.RawMessage(result))
			})
			if err := New(socket).TabRename(t.Context(), "a", "new"); !herdr.IsRefused(err) {
				t.Fatal(err)
			}
		})
	}
}

func TestLayoutCapabilities(t *testing.T) {
	features := New("").CapabilityStatus().Features
	if len(features) != 3 {
		t.Fatal(features)
	}
	for _, feature := range []string{herdr.FeaturePaneRead, herdr.FeatureTabRename, herdr.FeatureTabMove} {
		if features[feature].State != herdr.FeatureSupported {
			t.Fatal(features)
		}
	}
}
