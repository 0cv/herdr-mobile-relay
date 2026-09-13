package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func recentCacheEntry(id, text string) projectedEntry {
	return projectedEntry{Offset: int64(len(id)), Entry: Entry{
		ID: id, Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Text: text,
		Tools: []ToolActivity{{ID: "tool-1", Name: "Bash", Input: "pwd", Output: "done"}},
	}}
}

func TestRecentProjectionCacheClonesValuesAndTracksHits(t *testing.T) {
	cache := newRecentProjectionCache(BrowserOptions{
		RecentCacheEntries: 4, RecentCacheItemBytes: 1 << 20, RecentCacheBytes: 1 << 20, RecentCacheTTL: time.Minute,
	})
	value := recentProjection{
		Entries:     []projectedEntry{recentCacheEntry("entry-1", "answer")},
		Diagnostics: BrowseDiagnostics{OversizedRecords: 1},
		Plan:        &OMOTodoState{Available: true, Phases: []OMOTodoPhase{{Name: "phase", Tasks: []OMOTodoTask{{Content: "task"}}}}},
	}
	now := time.Unix(100, 0)
	if !cache.put("key", value, now) {
		t.Fatal("cache rejected a small projection")
	}
	got, ok := cache.get("key", now.Add(time.Second))
	if !ok {
		t.Fatal("cache missed a fresh projection")
	}
	got.Entries[0].Entry.Text = "mutated"
	got.Entries[0].Entry.Tools[0].Output = "mutated"
	got.Plan.Phases[0].Tasks[0].Content = "mutated"
	gotAgain, ok := cache.get("key", now.Add(2*time.Second))
	if !ok {
		t.Fatal("cache lost its immutable projection")
	}
	if gotAgain.Entries[0].Entry.Text != "answer" || gotAgain.Entries[0].Entry.Tools[0].Output != "done" || gotAgain.Plan.Phases[0].Tasks[0].Content != "task" {
		t.Fatalf("cached value was mutated: %#v", gotAgain)
	}
	hits, misses, projections := cache.stats()
	if hits != 2 || misses != 0 || projections != 0 {
		t.Fatalf("cache stats = %d hits, %d misses, %d projections", hits, misses, projections)
	}
}

func TestRecentProjectionCacheTTLAndLRUEviction(t *testing.T) {
	cache := newRecentProjectionCache(BrowserOptions{
		RecentCacheEntries: 2, RecentCacheItemBytes: 1 << 20, RecentCacheBytes: 1 << 20, RecentCacheTTL: 10 * time.Second,
	})
	base := time.Unix(200, 0)
	for index, key := range []string{"one", "two"} {
		if !cache.put(key, recentProjection{Entries: []projectedEntry{recentCacheEntry(key, key)}}, base.Add(time.Duration(index)*time.Second)) {
			t.Fatalf("put %s failed", key)
		}
	}
	if _, ok := cache.get("one", base.Add(3*time.Second)); !ok {
		t.Fatal("expected one to be present")
	}
	if !cache.put("three", recentProjection{Entries: []projectedEntry{recentCacheEntry("three", "three")}}, base.Add(4*time.Second)) {
		t.Fatal("put three failed")
	}
	if _, ok := cache.get("two", base.Add(5*time.Second)); ok {
		t.Fatal("least recently used entry survived")
	}
	if _, ok := cache.get("one", base.Add(5*time.Second)); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := cache.get("one", base.Add(16*time.Second)); ok {
		t.Fatal("idle entry survived TTL")
	}
}

func TestRecentProjectionCacheHonorsItemAndAggregateBudgets(t *testing.T) {
	cache := newRecentProjectionCache(BrowserOptions{
		RecentCacheEntries: 8, RecentCacheItemBytes: 1_000, RecentCacheBytes: 1_200, RecentCacheTTL: time.Minute,
	})
	tooLarge := recentProjection{Entries: []projectedEntry{{Entry: Entry{ID: "large", Text: strings.Repeat("x", 800)}}}}
	if cache.put("large", tooLarge, time.Unix(1, 0)) {
		t.Fatal("projection larger than the per-item budget was retained")
	}
	value := func(id string) recentProjection {
		return recentProjection{Entries: []projectedEntry{{Entry: Entry{ID: id, Role: "assistant", Text: strings.Repeat("x", 300)}}}}
	}
	if !cache.put("one", value("one"), time.Unix(2, 0)) || !cache.put("two", value("two"), time.Unix(3, 0)) {
		t.Fatal("budget-sized projection was rejected")
	}
	cache.mu.Lock()
	entries, bytes := len(cache.entries), cache.totalBytes
	cache.mu.Unlock()
	if entries > 1 || bytes > 1_200 {
		t.Fatalf("aggregate cache budget exceeded: entries=%d bytes=%d", entries, bytes)
	}
}

func TestRecentProjectionCacheConcurrentBookkeepingAndClear(t *testing.T) {
	cache := newRecentProjectionCache(BrowserOptions{
		RecentCacheEntries: 8, RecentCacheItemBytes: 1 << 20, RecentCacheBytes: 1 << 20, RecentCacheTTL: time.Minute,
	})
	var group sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for attempt := 0; attempt < 100; attempt++ {
				key := "key-" + string(rune('a'+worker%4))
				cache.put(key, recentProjection{Entries: []projectedEntry{recentCacheEntry(key, "text")}}, time.Unix(int64(attempt), 0))
				cache.get(key, time.Unix(int64(attempt), 0))
			}
		}(worker)
	}
	group.Wait()
	cache.clear()
	if len(cache.entries) != 0 || cache.totalBytes != 0 {
		t.Fatalf("cache was not cleared: %d entries, %d bytes", len(cache.entries), cache.totalBytes)
	}
}

func TestBrowserRecentProjectionCacheReusesVerifiedRange(t *testing.T) {
	reader, home := testReader(t)
	path := filepath.Join(home, ".claude", "projects", "-work", testSessionID+".jsonl")
	writeRows(t, path,
		map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"content": "question"}},
		map[string]any{"type": "assistant", "uuid": "a1", "message": map[string]any{"content": "answer"}},
	)
	options := DefaultBrowserOptions()
	options.RecentBytes = 1 << 20
	options.DefaultPageSize = 10
	options.MaxPageSize = 10
	browser, err := NewBrowser(reader, t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	scope := BrowseScope{Provider: "claude", CWD: "/work", SessionID: testSessionID}
	first, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
	if err != nil || len(first.Entries) != 2 {
		_ = browser.Close()
		t.Fatalf("first recent page = %#v, err = %v", first, err)
	}
	_, _, projections := browser.recentCache.stats()
	if projections != 1 {
		_ = browser.Close()
		t.Fatalf("projection applications after cold read = %d, want 1", projections)
	}
	if _, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	hits, _, projections := browser.recentCache.stats()
	if hits != 1 || projections != 1 {
		t.Fatalf("warm cache stats = %d hits, %d projections", hits, projections)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeRewrite, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(original), "answer", "rewrit", 1)
	if len(rewritten) != len(string(original)) {
		t.Fatal("test rewrite changed the source size")
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	// A caller may restore the modification time after an in-place rewrite.
	// The mutation token in the cache key must still force a fresh projection.
	if err := os.Chtimes(path, beforeRewrite.ModTime(), beforeRewrite.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10})
	if err != nil || len(changed.Entries) != 2 || changed.Entries[1].Text != "rewrit" {
		t.Fatalf("rewritten recent page = %#v, err = %v", changed, err)
	}
	_, _, projections = browser.recentCache.stats()
	if projections != 2 {
		t.Fatalf("projection applications after same-size rewrite = %d, want 2", projections)
	}
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	if len(browser.recentCache.entries) != 0 {
		t.Fatal("browser close retained recent projections")
	}
}

func BenchmarkRecentProjectionCacheGet(b *testing.B) {
	cache := newRecentProjectionCache(BrowserOptions{
		RecentCacheEntries: 4, RecentCacheItemBytes: 1 << 20, RecentCacheBytes: 1 << 20, RecentCacheTTL: time.Minute,
	})
	value := recentProjection{Entries: []projectedEntry{recentCacheEntry("entry", strings.Repeat("x", 256))}}
	now := time.Now()
	cache.put("key", value, now)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, ok := cache.get("key", now); !ok {
			b.Fatal("benchmark cache miss")
		}
	}
}
