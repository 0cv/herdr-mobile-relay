package conversation

import (
	"bytes"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func kimiWorkJournal(count, payload int, tools bool) []byte {
	var wire bytes.Buffer
	wire.WriteString("{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n")
	for i := 0; i < count; i++ {
		if tools {
			fmt.Fprintf(&wire, "{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"toolCalls\":[{\"type\":\"function\",\"id\":\"t%d\",\"name\":\"run\",\"arguments\":%q}]}}\n", i, strings.Repeat("a", payload))
			fmt.Fprintf(&wire, "{\"type\":\"context.append_message\",\"message\":{\"role\":\"tool\",\"toolCallId\":\"t%d\",\"content\":%q}}\n", i, strings.Repeat("b", payload))
		} else {
			fmt.Fprintf(&wire, "{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":%q}}\n", fmt.Sprintf("%08d:", i)+strings.Repeat("x", payload))
		}
	}
	return wire.Bytes()
}

func TestKimiBrowserColdWarmMeasurements(t *testing.T) {
	for _, test := range []struct {
		count, payload int
		tools          bool
	}{{384, 160, false}, {4096, 160, false}, {12000, 160, false}, {2048, 2048, false}, {1024, 2048, true}} {
		t.Run(fmt.Sprintf("%d-%d-tools-%t", test.count, test.payload, test.tools), func(t *testing.T) {
			wire := kimiWorkJournal(test.count, test.payload, test.tools)
			root, home := t.TempDir(), t.TempDir()
			t.Setenv(agentroots.KimiCodeListEnv, root)
			t.Setenv("KIMI_CODE_HOME", "")
			cwd, id := "/fixture/kimi-comparison", "session_comparison"
			kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Comparison", 1, wire)
			browser, err := NewBrowser(NewReader(home), t.TempDir(), DefaultBrowserOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer browser.Close()
			scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane"}
			var physical int64
			replays := 0
			browser.physicalReadObserver = func(n int64) { physical += n }
			browser.kimiProjectionObserver = func() { replays++ }
			read := func() BrowsePage {
				t.Helper()
				page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 24})
				if err != nil || !page.Available || len(page.Entries) != 24 {
					t.Fatalf("page=%+v err=%v", page, err)
				}
				return page
			}
			runtime.GC()
			var before, cold, retained, warm runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			first := read()
			coldElapsed := time.Since(start)
			runtime.ReadMemStats(&cold)
			coldBytes := physical
			if coldBytes != int64(len(wire)) || replays != 1 || browser.kimiProjection == nil {
				t.Fatal("cold verification/cache did not match fixture")
			}
			runtime.GC()
			runtime.ReadMemStats(&retained)
			physical = 0
			start = time.Now()
			second := read()
			warmElapsed := time.Since(start)
			runtime.ReadMemStats(&warm)
			if physical != 0 || replays != 1 || !reflect.DeepEqual(first.Entries, second.Entries) {
				t.Fatal("warm content/read mismatch")
			}
			t.Logf("KIMI_BROWSER_MEASUREMENT count=%d payload=%d tools=%t cold_bytes=%d warm_bytes=%d cold_alloc_bytes=%d warm_alloc_bytes=%d retained_heap_delta=%d cold_elapsed_us=%d warm_elapsed_us=%d", test.count, test.payload, test.tools, coldBytes, physical, cold.TotalAlloc-before.TotalAlloc, warm.TotalAlloc-retained.TotalAlloc, int64(retained.HeapAlloc)-int64(before.HeapAlloc), coldElapsed.Microseconds(), warmElapsed.Microseconds())
			runtime.KeepAlive(browser)
			runtime.KeepAlive(first)
		})
	}
}
