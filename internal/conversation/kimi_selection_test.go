package conversation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKimiInferredSelectionCacheIdleAndInvalidation(t *testing.T) {
	for _, count := range []int{1, 64, 1024, 2047} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".kimi-code")
			bucket := filepath.Join(root, "sessions", kimiWorkspaceKey("/fixture"))
			for i := range count {
				id := fmt.Sprintf("session_%04d", i)
				dir := filepath.Join(bucket, id)
				if err := os.MkdirAll(filepath.Join(dir, "agents", "main"), 0700); err != nil {
					t.Fatal(err)
				}
				state, _ := json.Marshal(map[string]any{"id": id, "version": 2, "cwd": "/fixture", "updatedAt": i + 1})
				if err := os.WriteFile(filepath.Join(dir, "state.json"), state, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agents", "main", "wire.jsonl"), []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			reader := NewReader(home)
			project := ProjectContext{CWD: "/fixture"}
			want := fmt.Sprintf("session_%04d", count-1)
			if id, _, code := reader.SelectKimiSession(project, ""); id != want || code != "" {
				t.Fatalf("id=%s code=%s", id, code)
			}
			var key string
			var cached kimiSelection
			for k, v := range reader.kimiSelections {
				key, cached = k, v
			}
			if len(cached.files) > maxKimiSelectionPaths {
				t.Fatal("unbounded watch set")
			}
			start := time.Now()
			for range 100 {
				if id, _, code := reader.SelectKimiSession(project, ""); id != want || code != "" {
					t.Fatalf("id=%s code=%s", id, code)
				}
			}
			if &reader.kimiSelections[key].files[0] != &cached.files[0] {
				t.Fatal("idle check repeated discovery")
			}
			t.Logf("sessions=%d paths=%d ticks=100 average=%s", count, len(cached.files), time.Since(start)/100)
			statePath := filepath.Join(bucket, want, "state.json")
			info, err := os.Stat(statePath)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			for i, b := range data {
				if b == 'f' {
					data[i] = 'x'
					break
				}
			}
			if err := os.WriteFile(statePath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(statePath, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if id, _, _ := reader.SelectKimiSession(project, ""); id == want {
				t.Fatal("same-size restored-mtime state edit missed")
			}
		})
	}
}
