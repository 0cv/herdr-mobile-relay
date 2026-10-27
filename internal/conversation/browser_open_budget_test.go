package conversation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func syntheticCodexHistory(t *testing.T, count, payload int) (*Reader, BrowseScope, int) {
	t.Helper()
	reader, home := testReader(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "08", "12", "rollout-2026-08-12T10-00-00-"+testSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	lineBytes := 0
	for index := 0; index < count; index++ {
		line := fmt.Sprintf("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"%08d:%s\"}]}}\n", index, strings.Repeat("x", payload))
		lineBytes = len(line)
		if _, err := writer.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return reader, BrowseScope{Provider: "codex", CWD: "/work", SessionID: testSessionID}, lineBytes
}

func TestRecentOpeningWorkBudget(t *testing.T) {
	reader, scope, lineBytes := syntheticCodexHistory(t, 24000, 1024)
	type work struct {
		Mode          string `json:"mode"`
		ColdBytes     int64  `json:"cold_bytes"`
		WarmBytes     int64  `json:"warm_bytes"`
		ColdParsed    int64  `json:"cold_parsed_records"`
		WarmParsed    int64  `json:"warm_parsed_records"`
		ColdMicros    int64  `json:"cold_microseconds"`
		WarmMicros    int64  `json:"warm_microseconds"`
		WireEntries   int    `json:"wire_entries"`
		ResponseBytes int    `json:"response_bytes"`
	}
	measure := func(name string, recent int64, limit int) work {
		options := DefaultBrowserOptions()
		options.RecentBytes = recent
		browser, err := NewBrowser(reader, t.TempDir(), options)
		if err != nil {
			t.Fatal(err)
		}
		defer browser.Close()
		var bytes int64
		browser.physicalReadObserver = func(n int64) { bytes += n }
		start := time.Now()
		first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: limit})
		cold := time.Since(start)
		if err != nil || first.Error != nil || len(first.Entries) != limit || !first.HasMore || first.Mode != BrowseRecent {
			t.Fatalf("initial page = %#v, err = %v", first, err)
		}
		if len(browser.jobs) != 0 {
			t.Fatal("opening prepared deep history")
		}
		coldBytes := bytes
		bytes = 0
		start = time.Now()
		warm, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: limit})
		warmTime := time.Since(start)
		if err != nil || len(warm.Entries) != limit || warm.Entries[0].ID != first.Entries[0].ID {
			t.Fatal("warm page changed identity")
		}
		_, _, projections := browser.recentCache.stats()
		warmParsed := int64(0)
		if projections > 1 {
			warmParsed = recent / int64(lineBytes)
		}
		encoded, _ := json.Marshal(first)
		result := work{name, coldBytes, bytes, recent / int64(lineBytes), warmParsed, cold.Microseconds(), warmTime.Microseconds(), limit, len(encoded)}
		raw, _ := json.Marshal(result)
		t.Log(string(raw))
		return result
	}
	before := measure("before-16MiB-200", maxConversationBytes, 200)
	after := measure("after-256KiB-24", DefaultBrowserOptions().RecentBytes, 24)
	if after.ColdBytes > 2*256*1024+64*1024+1 || after.WarmBytes > 256*1024+64*1024 {
		t.Fatalf("opening exceeded acquisition budget: %#v", after)
	}
	if after.ColdBytes*20 >= before.ColdBytes || after.ColdParsed*20 >= before.ColdParsed || after.WarmParsed != 0 || after.ResponseBytes*5 >= before.ResponseBytes {
		t.Fatalf("work not reduced: before=%#v after=%#v", before, after)
	}
}

func TestSmallRecentWindowPagesHaveNoGapsOrDuplicates(t *testing.T) {
	reader, scope, _ := syntheticCodexHistory(t, 1600, 200)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	cursor := ""
	seen := make(map[string]bool)
	last := 1600
	for attempts := 0; attempts < 10000; attempts++ {
		page, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Cursor: cursor, Limit: 24})
		if err != nil || page.Error != nil {
			t.Fatalf("page = %#v, err = %v", page, err)
		}
		if page.State == BrowsePreparing {
			cursor = page.NextCursor
			time.Sleep(time.Millisecond)
			continue
		}
		for index := len(page.Entries) - 1; index >= 0; index-- {
			entry := page.Entries[index]
			last--
			if seen[entry.ID] || !strings.HasPrefix(entry.Text, fmt.Sprintf("%08d:", last)) {
				t.Fatalf("gap or duplicate at %d: %#v", last, entry)
			}
			seen[entry.ID] = true
		}
		if !page.HasMore {
			if last != 0 || len(seen) != 1600 {
				t.Fatalf("incomplete history: last=%d seen=%d", last, len(seen))
			}
			return
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	t.Fatal("paging work did not finish")
}
