package conversation

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const agySessionID = "071dc833-ce1a-4df3-b198-03af792b4ae0"

func pbVarintField(num int, value uint64) []byte {
	out := binary.AppendUvarint(nil, uint64(num)<<3)
	return binary.AppendUvarint(out, value)
}

func pbBytesField(num int, value []byte) []byte {
	out := binary.AppendUvarint(nil, uint64(num)<<3|2)
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}

func pbJoin(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func agyMeta(seconds uint64, callID string) []byte {
	stamp := pbBytesField(1, pbJoin(pbVarintField(1, seconds), pbVarintField(2, 500)))
	if callID != "" {
		stamp = append(stamp, pbBytesField(4, pbBytesField(1, []byte(callID)))...)
	}
	return stamp
}

func agyUserStep(seconds uint64, text string) []byte {
	return pbJoin(pbVarintField(1, agyStepUser), pbBytesField(5, agyMeta(seconds, "")),
		pbBytesField(19, pbBytesField(2, []byte(text))))
}

func agyAssistantStep(seconds uint64, text string, calls ...[3]string) []byte {
	message := []byte{}
	if text != "" {
		message = pbBytesField(1, []byte(text))
	}
	message = append(message, pbBytesField(3, []byte("private thinking"))...)
	for _, call := range calls {
		message = append(message, pbBytesField(7, pbJoin(
			pbBytesField(1, []byte(call[0])), pbBytesField(2, []byte(call[1])), pbBytesField(3, []byte(call[2])),
		))...)
	}
	return pbJoin(pbVarintField(1, agyStepAssistant), pbBytesField(5, agyMeta(seconds, "")), pbBytesField(20, message))
}

func agyToolDoneStep(seconds uint64, callID string) []byte {
	return pbJoin(pbVarintField(1, agyStepToolDone), pbBytesField(5, agyMeta(seconds, callID)))
}

type agyStep struct {
	typ      int
	payload  []byte
	metadata []byte
}

func createAgyDatabase(t *testing.T, sqlite, database string, steps []agyStep) {
	t.Helper()
	var sql strings.Builder
	sql.WriteString("BEGIN;CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,`status` integer NOT NULL DEFAULT 0,`metadata` blob,`step_payload` blob,PRIMARY KEY (`idx`));")
	for index, step := range steps {
		metadata := step.metadata
		if metadata == nil {
			metadata = []byte{}
		}
		fmt.Fprintf(&sql, "INSERT INTO steps(idx,step_type,status,metadata,step_payload) VALUES(%d,%d,3,X'%x',X'%x');", index, step.typ, metadata, step.payload)
	}
	sql.WriteString("COMMIT;")
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(sql.String())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create agy database: %v: %s", err, output)
	}
}

func agyFixture(t *testing.T) (*Reader, string, string) {
	t.Helper()
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 is unavailable")
	}
	root := t.TempDir()
	t.Setenv(agentroots.AgyListEnv, root)
	// agentroots appends the conversations leaf to every configured base.
	conversations := filepath.Join(root, "conversations")
	if err := os.MkdirAll(conversations, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(conversations, agySessionID+".db")
	call := [3]string{"call_1", "run_command", `{"CommandLine":"ls -la"}`}
	createAgyDatabase(t, sqlite, database, []agyStep{
		{typ: agyStepUser, payload: agyUserStep(1000, "check the gateway")},
		{typ: agyStepAssistant, payload: agyAssistantStep(1001, "", call)},
		{typ: agyStepToolDone, payload: agyToolDoneStep(1002, "call_1"), metadata: agyMeta(1002, "call_1")},
		{typ: 23, payload: pbVarintField(1, 23)},
		{typ: agyStepAssistant, payload: agyAssistantStep(1003, "It was the memory watchdog.", [3]string{"call_2", "read_file", `{"AbsolutePath":"/tmp/x"}`})},
		{typ: agyStepAssistant, payload: agyAssistantStep(1004, "Done.")},
	})
	reader := NewReader(t.TempDir())
	reader.agy.binary = sqlite
	return reader, database, agySessionID
}

func TestAgyReaderProjectsConversation(t *testing.T) {
	reader, _, session := agyFixture(t)
	for _, name := range []string{"agy", "Antigravity", "antigravity-cli"} {
		if !Supported(name) {
			t.Fatalf("Supported(%q) = false", name)
		}
	}
	page, err := reader.ReadFor("agy", t.TempDir(), session, "", 80)
	if err != nil || !page.Available || page.SourceCorrupt {
		t.Fatalf("page = %#v, err = %v", page, err)
	}
	if len(page.Entries) != 4 {
		t.Fatalf("entries = %d, want 4 (tool-result and type 23 steps skipped): %#v", len(page.Entries), page.Entries)
	}
	user, firstCall, secondCall, final := page.Entries[0], page.Entries[1], page.Entries[2], page.Entries[3]
	if user.Role != "user" || user.Text != "check the gateway" || user.ID != "0" || user.Timestamp == "" {
		t.Fatalf("user entry = %#v", user)
	}
	if len(firstCall.Tools) != 1 || firstCall.Tools[0].Name != "run_command" || firstCall.Tools[0].State != "completed" ||
		!strings.Contains(firstCall.Tools[0].Input, "ls -la") {
		t.Fatalf("completed tool = %#v", firstCall.Tools)
	}
	if secondCall.Text != "It was the memory watchdog." || len(secondCall.Tools) != 1 || secondCall.Tools[0].State != "running" {
		t.Fatalf("running tool entry = %#v", secondCall)
	}
	if final.Role != "assistant" || final.Text != "Done." || strings.Contains(final.Text, "private thinking") {
		t.Fatalf("final entry = %#v", final)
	}
}

func TestAgyReaderPagesBackwardByStep(t *testing.T) {
	reader, _, session := agyFixture(t)
	latest, err := reader.ReadFor("agy", "", session, "", 2)
	if err != nil || len(latest.Entries) != 2 || !latest.HasMore || latest.Entries[1].Text != "Done." {
		t.Fatalf("latest = %#v, err = %v", latest, err)
	}
	older, err := reader.ReadFor("agy", "", session, latest.Entries[0].ID, 10)
	if err != nil || older.HasMore || len(older.Entries) != 2 || older.Entries[0].Text != "check the gateway" {
		t.Fatalf("older = %#v, err = %v", older, err)
	}
	bad, err := reader.ReadFor("agy", "", session, "nope", 10)
	if err != nil || bad.Available || bad.ReasonCode != "invalid_cursor" {
		t.Fatalf("bad cursor = %#v, err = %v", bad, err)
	}
}

func TestAgyReaderRejectsUnsafeSessions(t *testing.T) {
	reader, _, _ := agyFixture(t)
	for _, session := range []string{"", "../etc/passwd", "not-a-uuid", "11111111-1111-1111-1111-111111111111"} {
		page, err := reader.ReadFor("agy", "", session, "", 10)
		if err != nil || page.Available || page.ReasonCode != "invalid_session" {
			t.Fatalf("session %q page = %#v, err = %v", session, page, err)
		}
	}
}

func TestAgyBrowserNativeCursorAndChangeToken(t *testing.T) {
	reader, database, session := agyFixture(t)
	browser, err := NewBrowser(reader, t.TempDir(), DefaultBrowserOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	scope := BrowseScope{Provider: "agy", SessionID: session}
	latest, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 2})
	if err != nil || latest.Mode != BrowseNative || len(latest.Entries) != 2 || !latest.HasMore || latest.NextCursor == "" {
		t.Fatalf("latest = %#v, err = %v", latest, err)
	}
	older, err := browser.ReadPage(context.Background(), BrowseRequest{Scope: scope, Limit: 10, Cursor: latest.NextCursor})
	if err != nil || len(older.Entries) != 2 || older.HasMore || older.Entries[0].Text != "check the gateway" {
		t.Fatalf("older = %#v, err = %v", older, err)
	}

	before, err := reader.ChangeToken(context.Background(), scope)
	if err != nil || before == "" {
		t.Fatalf("change token = %q, err = %v", before, err)
	}
	if again, _ := reader.ChangeToken(context.Background(), scope); again != before {
		t.Fatal("idle change token moved")
	}
	sqlite, _ := exec.LookPath("sqlite3")
	command := exec.Command(sqlite, database)
	command.Stdin = strings.NewReader(fmt.Sprintf("INSERT INTO steps(idx,step_type,status,metadata,step_payload) VALUES(99,%d,3,X'',X'%x');", agyStepUser, agyUserStep(2000, "again")))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("append step: %v: %s", err, output)
	}
	if after, _ := reader.ChangeToken(context.Background(), scope); after == before {
		t.Fatal("appended step did not change the token")
	}
}

func TestParsePBRejectsMalformedInput(t *testing.T) {
	for _, data := range [][]byte{{0x0a, 0x05, 'a'}, {0x80}, {0x00}, {0x0b}} {
		if _, ok := parsePB(data); ok {
			t.Fatalf("parsePB(%x) accepted malformed input", data)
		}
	}
}
