package conversation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

func kimiSample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "kimi", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestKimiResearchGoldenProjections(t *testing.T) {
	var manifest struct {
		Cases []struct {
			ID       string   `json:"id"`
			Wire     string   `json:"wire"`
			Expected []string `json:"expectedTextInOrder"`
			Final    []string `json:"expectedFinalTextInOrder"`
			Tools    []struct {
				Name   string `json:"name"`
				Input  any    `json:"input"`
				Output string `json:"output"`
			} `json:"expectedToolExchanges"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(kimiSample(t, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, sample := range manifest.Cases {
		if sample.Wire == "" {
			continue
		}
		t.Run(sample.ID, func(t *testing.T) {
			result := projectKimi(t.Context(), kimiSample(t, sample.Wire))
			if sample.ID == "builtin-import" {
				if result.code != "kimi_imported" || len(result.entries) != 0 {
					t.Fatalf("import policy: %+v", result)
				}
				return
			}
			if result.code != "" {
				t.Fatalf("projection unavailable: %s", result.code)
			}
			texts := []string{}
			tools := []ToolActivity{}
			for _, entry := range result.entries {
				if entry.Text != "" {
					texts = append(texts, entry.Text)
				}
				tools = append(tools, entry.Tools...)
			}
			expected := sample.Expected
			if len(sample.Final) > 0 {
				expected = sample.Final
			}
			if !reflect.DeepEqual(texts, expected) {
				t.Fatalf("text=%q want=%q", texts, expected)
			}
			if sample.ID != "interrupted" && len(tools) != len(sample.Tools) {
				t.Fatalf("tools=%+v", tools)
			}
			for i, expected := range sample.Tools {
				var input any
				if json.Unmarshal([]byte(tools[i].Input), &input) != nil || !reflect.DeepEqual(input, expected.Input) || tools[i].Name != expected.Name || tools[i].Output != expected.Output || tools[i].State != "completed" {
					t.Fatalf("tool=%+v want=%+v", tools[i], expected)
				}
			}
			if sample.ID == "interrupted" && (!result.diagnostics.ContinuationIncomplete || len(tools) != 1 || tools[0].State != "running") {
				t.Fatalf("interrupted=%+v", result)
			}
			raw, _ := json.Marshal(result.entries)
			for _, sentinel := range []string{"DO_NOT_EXPORT_", "ORPHAN_PART", "ABANDONED_", "WITHDRAWN_", "blobref:", "https://"} {
				if bytes.Contains(raw, []byte(sentinel)) {
					t.Fatalf("privacy/withdrawal leak: %s", sentinel)
				}
			}
		})
	}
}

func TestKimiWorkspaceHashVectors(t *testing.T) {
	var vectors struct {
		V2 []struct {
			Input       string
			Normalized  string
			Slug        string
			ExpectedKey string
		}
	}
	if err := json.Unmarshal(kimiSample(t, "hash-cases.json"), &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors.V2 {
		t.Run(vector.Input, func(t *testing.T) {
			hash := sha256.Sum256([]byte(vector.Normalized))
			want := fmt.Sprintf("wd_%s_%x", vector.Slug, hash[:6])
			if vector.ExpectedKey != "" {
				want = vector.ExpectedKey
			}
			if got := kimiWorkspaceKey(vector.Input); got != want {
				t.Fatalf("got=%q want=%q", got, want)
			}
		})
	}
}

func TestKimiMalformedAndUnsupportedDerivatives(t *testing.T) {
	header := "{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n"
	branches := string(kimiSample(t, "v2.2.0-branches/wire.jsonl"))
	lines := strings.Split(strings.TrimSpace(branches), "\n")
	cases := map[string]string{
		"malformed":             header + "{broken}\n",
		"invalid utf8":          header + "{\"type\":\"x\",\"text\":\"\xff\"}\n",
		"oversized":             header + `{"type":"context.append_message","message":{"role":"user","content":"` + strings.Repeat("x", maxKimiRecord) + `"}}` + "\n",
		"deep":                  header + strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33) + "\n",
		"unknown structural":    header + "{\"type\":\"context.replace\"}\n",
		"human only":            header + "{\"type\":\"agent.message.appended\",\"message\":{\"message\":{\"role\":\"assistant\",\"content\":\"DO_NOT_EXPORT_HUMAN\"}}}\n",
		"switch prefix":         strings.Join(lines[:10], "\n") + "\n",
		"switch pair prefix":    strings.Join(lines[:11], "\n") + "\n",
		"unknown base":          strings.Replace(branches, `"base":{"branch":"main","line":5}`, `"base":{"branch":"unknown","line":5}`, 1),
		"cyclic":                strings.Replace(branches, `"base":{"branch":"main","line":5}`, `"base":{"branch":"b1","line":5}`, 1),
		"range":                 strings.Replace(branches, `"base":{"branch":"main","line":5}`, `"base":{"branch":"main","line":999}`, 1),
		"duplicate branch":      strings.Replace(branches, `"branch":"b2"`, `"branch":"b1"`, 1),
		"wrong paired count":    strings.Replace(branches, `"count":1`, `"count":2`, 1),
		"wrong paired line":     strings.Replace(branches, `"legacyUndoLine":11`, `"legacyUndoLine":12`, 1),
		"malformed switch time": strings.Replace(branches, `"time":1791936003000`, `"time":{"bad":"value"}`, 1),
		"subagent":              strings.Replace(branches, `"agentId":"main"`, `"agentId":"other"`, 1),
		"unpaired undone":       header + "{\"type\":\"context.undone\",\"turns\":1}\n",
		"bad undo":              header + "{\"type\":\"context.undo\",\"count\":1}\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			result := projectKimi(t.Context(), []byte(raw))
			if result.code == "" || len(result.entries) != 0 {
				t.Fatalf("must fail closed: %+v", result)
			}
		})
	}
	withCandidate := strings.Replace(branches, lines[1], "42\n"+lines[1], 1)
	withCandidate = strings.ReplaceAll(withCandidate, `"line":5`, `"line":6`)
	withCandidate = strings.ReplaceAll(withCandidate, `"line":12`, `"line":13`)
	withCandidate = strings.ReplaceAll(withCandidate, `"legacyUndoLine":11`, `"legacyUndoLine":12`)
	withCandidate = strings.ReplaceAll(withCandidate, `"legacyUndoLine":18`, `"legacyUndoLine":19`)
	if result := projectKimi(t.Context(), []byte(withCandidate)); result.code != "" || len(result.entries) != 4 {
		t.Fatalf("candidate line numbering: %+v", result)
	}
}

func TestKimiPartialUTF8AndMutationCheckpoints(t *testing.T) {
	header := []byte("{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n")
	line := []byte("{\"type\":\"context.append_message\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"café\"}]}}\n")
	for _, cut := range []int{4, bytes.IndexByte(line, 0xc3) + 1, len(line) - 1} {
		partial := projectKimi(t.Context(), append(append([]byte{}, header...), line[:cut]...))
		if partial.code != "" || len(partial.entries) != 0 || !partial.diagnostics.ContinuationIncomplete {
			t.Fatalf("partial=%+v", partial)
		}
	}
	complete := projectKimi(t.Context(), append(header, line...))
	if complete.code != "" || len(complete.entries) != 1 || complete.entries[0].Text != "café" {
		t.Fatalf("complete=%+v", complete)
	}
	for _, file := range []string{"v0.36.1-mutations/wire.jsonl", "v2.2.0-branches/wire.jsonl"} {
		lines := bytes.SplitAfter(kimiSample(t, file), []byte("\n"))
		for _, count := range []int{7, 8, 11} {
			if strings.Contains(file, "branches") {
				continue
			}
			result := projectKimi(t.Context(), bytes.Join(lines[:count], nil))
			if result.code != "" || len(result.entries) != 2 {
				t.Fatalf("checkpoint %d: %+v", count, result)
			}
		}
	}
}

func kimiFixture(t *testing.T, root, bucket, id, cwd, title string, updated int64, wire []byte) string {
	t.Helper()
	dir := filepath.Join(root, "sessions", bucket, id)
	path := filepath.Join(dir, "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, wire, 0600); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{"id": id, "version": 2, "cwd": cwd, "updatedAt": updated, "title": title, "archived": false})
	if err := os.WriteFile(filepath.Join(dir, "state.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKimiLocatorIdentityAndBoundaries(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root+string(os.PathListSeparator)+root)
	t.Setenv("KIMI_CODE_HOME", "")
	reader := NewReader(home)
	cwd, id := "/fixture/kimi-project", "session_fixture"
	wire := kimiSample(t, "v0.36.1-v2/wire.jsonl")
	path := kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "First", 1, wire)
	project := ProjectContext{CWD: "/not-the-cwd", ForegroundCWD: cwd}
	selected, location, code := reader.SelectKimiSession(project, id)
	if code != "" || selected != id || location.Path != path || location.Title != "First" {
		t.Fatalf("selection=%q %+v %s", selected, location, code)
	}
	other := kimiFixture(t, root, "registered_workspace", "session_newer", cwd, "Newer", 10, wire)
	if selected, location, code := reader.SelectKimiSession(project, ""); code != "" || selected != "session_newer" || location.Path != other {
		t.Fatalf("recency=%q %+v %s", selected, location, code)
	}
	for _, bad := range []string{"stale", "../escape", "a/b", "ses_imported"} {
		if _, _, code := reader.SelectKimiSession(project, bad); code == "" {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, _, code := reader.SelectKimiSession(ProjectContext{CWD: "/wrong"}, id); code == "" {
		t.Fatal("cwd mismatch")
	}
	page, err := reader.ReadFor("kimi", cwd, "", "", 200)
	if err != nil || page.Available {
		t.Fatalf("sessionless=%+v %v", page, err)
	}
	second := t.TempDir()
	kimiFixture(t, second, kimiWorkspaceKey(cwd), id, cwd, "Second", 100, wire)
	t.Setenv(agentroots.KimiCodeListEnv, root+string(os.PathListSeparator)+second)
	if _, location, code := reader.SelectKimiSession(project, id); code != "" || location.Title != "First" {
		t.Fatalf("root ordering=%+v %s", location, code)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, wire, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv(agentroots.KimiCodeListEnv, root)
	if _, _, code := reader.SelectKimiSession(project, id); code == "" {
		t.Fatal("outside-root symlink accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, code := reader.SelectKimiSession(project, id); code == "" {
		t.Fatal("nonregular source accepted")
	}
	kimiFixture(t, root, kimiWorkspaceKey(cwd), "session_bad", cwd, "Bad", 1, wire)
	statePath := filepath.Join(root, "sessions", kimiWorkspaceKey(cwd), "session_bad", "state.json")
	if err := os.WriteFile(statePath, []byte(`{"id":"different","version":2,"cwd":"/fixture/kimi-project"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := reader.SelectKimiSession(project, "session_bad"); code == "" {
		t.Fatal("directory/state ID mismatch")
	}
	index := fmt.Sprintf("{\"sessionId\":\"session_newer\",\"sessionDir\":%q,\"workDir\":\"/wrong\"}\n", filepath.Dir(filepath.Dir(filepath.Dir(other))))
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte(index), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := reader.SelectKimiSession(project, "session_newer"); code == "" {
		t.Fatal("index contradiction")
	}
}

func TestKimiBrowserAppendRewriteAndCursorFences(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd, id := "/fixture/kimi-project", "session_browser"
	wire := kimiSample(t, "rewrites/before.jsonl")
	path := kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Browser", 1, wire)
	reader := NewReader(home)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "kimi", CWD: cwd, SessionID: id, PaneID: "pane", ProcessIdentity: "100:argv"}
	read := func(cursor string) BrowsePage {
		t.Helper()
		page, err := browser.ReadPage(t.Context(), BrowseRequest{Scope: scope, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	first := read("")
	if !first.Available || len(first.Entries) != 1 {
		t.Fatalf("first=%+v", first)
	}
	appendLine := []byte("{\"type\":\"context.append_message\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"Appended.\"}]}}\n")
	if err := os.WriteFile(path, append(append([]byte{}, wire...), appendLine...), 0600); err != nil {
		t.Fatal(err)
	}
	second := read("")
	if second.SourceRevision != first.SourceRevision || !second.HasMore || second.NextCursor == "" {
		t.Fatalf("append=%+v", second)
	}
	older := read(second.NextCursor)
	if older.Entries[0].ID != first.Entries[0].ID {
		t.Fatal("append changed stable identity")
	}
	stamp, _ := reader.ChangeToken(t.Context(), scope)
	info, _ := os.Stat(path)
	rewritten := append(append([]byte{}, kimiSample(t, "rewrites/after.jsonl")...), appendLine...)
	if err := os.WriteFile(path, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changedStamp, _ := reader.ChangeToken(t.Context(), scope)
	if stamp == changedStamp {
		t.Fatal("same-size rewrite token did not change")
	}
	changed := read("")
	if changed.SourceRevision == first.SourceRevision {
		t.Fatal("older rewrite must reset tail even when latest entry is unchanged")
	}
	if stale := read(second.NextCursor); stale.ReasonCode != "source_changed" {
		t.Fatalf("stale=%+v", stale)
	}
	beforeMutation := changed.SourceRevision
	if err := os.WriteFile(path, append(rewritten, []byte("{\"type\":\"context.undo\",\"count\":1}\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	undo := read("")
	if undo.SourceRevision == beforeMutation || len(undo.Entries) != 0 {
		t.Fatalf("undo=%+v", undo)
	}
	scope.ProcessIdentity = "101:new"
	if stale := read(changed.NextCursor); stale.ReasonCode != "invalid_cursor" {
		t.Fatalf("process cursor fence=%+v", stale)
	}
}

func TestKimiReaderPublicAdmission(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".kimi-code")
	cwd, id := "/fixture/kimi-project", "session_public"
	kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Public", 1, kimiSample(t, "v0.36.1-v2/wire.jsonl"))
	page, err := NewReader(home).ReadFor("kimi", cwd, id, "", 200)
	if err != nil || !page.Available || len(page.Entries) != 6 {
		t.Fatalf("Kimi reader admission: %+v %v", page, err)
	}
}

func TestKimiOriginsCompactionAndBounds(t *testing.T) {
	header := "{\"type\":\"metadata\",\"protocol_version\":\"1.5\"}\n"
	prompt := func(text string, origin any) string {
		raw, _ := json.Marshal(map[string]any{"type": "context.append_message", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}, "origin": origin}})
		return string(raw) + "\n"
	}
	wire := header + prompt("Keep", nil)
	for _, kind := range []string{"injection", "hook", "skill", "plugin", "compaction", "system", "task", "cron", "retry", "shell"} {
		wire += prompt("DO_NOT_EXPORT_"+kind, map[string]any{"kind": kind, "name": "DO_NOT_EXPORT_NAME", "args": "DO_NOT_EXPORT_ARGS"})
	}
	wire += prompt("Steer", map[string]any{"kind": "user", "inTurn": true}) + "{\"type\":\"context.undo\",\"count\":1}\n"
	result := projectKimi(t.Context(), []byte(wire))
	if result.code != "" || len(result.entries) != 0 {
		t.Fatalf("origin/steer undo=%+v", result)
	}
	wire = header + prompt("Kept anchor", nil) + "{\"type\":\"context.apply_compaction\",\"compactedCount\":0,\"keptUserMessageCount\":1}\n" + prompt("New anchor", nil) + "{\"type\":\"context.undo\",\"count\":1}\n"
	result = projectKimi(t.Context(), []byte(wire))
	if result.code != "" || len(result.entries) != 1 || result.entries[0].Text != "Kept anchor" {
		t.Fatalf("compaction floor=%+v", result)
	}
	if result := projectKimi(t.Context(), make([]byte, maxConversationBytes+1)); result.code != "output_limit" {
		t.Fatal("unbounded replay admitted")
	}
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(agentroots.KimiCodeListEnv, root)
	t.Setenv("KIMI_CODE_HOME", "")
	cwd := "/fixture/kimi-project"
	id := "session_archived"
	path := kimiFixture(t, root, kimiWorkspaceKey(cwd), id, cwd, "Archived", 100, kimiSample(t, "v0.36.1-v2/wire.jsonl"))
	statePath := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "state.json")
	data, _ := os.ReadFile(statePath)
	data = bytes.Replace(data, []byte(`"archived":false`), []byte(`"archived":true`), 1)
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(home)
	if _, _, code := reader.SelectKimiSession(ProjectContext{CWD: cwd}, ""); code == "" {
		t.Fatal("archived session selected by recency")
	}
	if _, _, code := reader.SelectKimiSession(ProjectContext{CWD: cwd}, id); code != "" {
		t.Fatal("reported archived ID rejected")
	}
}

func TestKimiCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := projectKimi(ctx, kimiSample(t, "v0.36.1-v2/wire.jsonl")); result.code != "request_cancelled" {
		t.Fatal(result.code)
	}
}
