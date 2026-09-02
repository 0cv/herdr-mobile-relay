package upload

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

func uploadTarget(generation int64) protocol.TargetRef {
	return protocol.TargetRef{ServerSessionID: "session", PaneID: "pane", TerminalID: "terminal", Generation: generation}
}

func digestFor(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func newTestManager(t *testing.T, now *time.Time) *Manager {
	t.Helper()
	manager, err := NewManager(Config{
		Root: t.TempDir(), ChunkBytes: 1024, MaxFiles: 3, MaxFileBytes: 4096,
		MaxBatchBytes: 8192, SessionTTL: time.Minute, AttachmentTTL: time.Hour,
		Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func beginOne(t *testing.T, manager *Manager, target protocol.TargetRef, name, mediaType string, size int64) BeginResult {
	t.Helper()
	result, err := manager.Begin(BeginRequest{Target: target, Files: []FileSpec{{Name: name, MediaType: mediaType, Bytes: size}}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func sendChunk(t *testing.T, manager *Manager, target protocol.TargetRef, id string, fileIndex, sequence int, data []byte) {
	t.Helper()
	if _, err := manager.Chunk(ChunkRequest{
		Target: target, UploadID: id, FileIndex: fileIndex, Sequence: sequence,
		Data: data, SHA256: digestFor(data),
	}); err != nil {
		t.Fatal(err)
	}
}

func errorCode(t *testing.T, err error) string {
	t.Helper()
	var uploadError *Error
	if !errors.As(err, &uploadError) {
		t.Fatalf("error = %T %v, want *upload.Error", err, err)
	}
	return uploadError.Code
}
func documentArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestUploadAcceptsAdvertisedDocumentFormats(t *testing.T) {
	now := time.Now()
	target := uploadTarget(0)
	tests := []struct {
		name      string
		filename  string
		mediaType string
		content   []byte
	}{
		{"json", "data.json", "application/json", []byte(`{"ok":true}`)},
		{"csv", "data.csv", "text/csv", []byte("name,value\none,1\n")},
		{"docx", "document.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			documentArchive(t, map[string]string{"[Content_Types].xml": "<Types/>", "word/document.xml": "<document/>"})},
		{"odt", "document.odt", "application/vnd.oasis.opendocument.text",
			documentArchive(t, map[string]string{"mimetype": "application/vnd.oasis.opendocument.text", "content.xml": "<document/>"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newTestManager(t, &now)
			begin := beginOne(t, manager, target, test.filename, test.mediaType, int64(len(test.content)))
			sendChunk(t, manager, target, begin.UploadID, 0, 0, test.content)
			result, err := manager.Finish(FinishRequest{
				Target:   target,
				UploadID: begin.UploadID,
				Files:    []FileDigest{{FileIndex: 0, SHA256: digestFor(test.content)}},
			})
			if err != nil || len(result.Attachments) != 1 {
				t.Fatalf("finish = %#v, %v", result, err)
			}
		})
	}
}

func TestUploadRejectsMismatchedDocumentContainer(t *testing.T) {
	now := time.Now()
	manager := newTestManager(t, &now)
	target := uploadTarget(0)
	content := documentArchive(t, map[string]string{"xl/workbook.xml": "<workbook/>"})
	begin := beginOne(
		t,
		manager,
		target,
		"document.docx",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		int64(len(content)),
	)
	sendChunk(t, manager, target, begin.UploadID, 0, 0, content)
	_, err := manager.Finish(FinishRequest{
		Target:   target,
		UploadID: begin.UploadID,
		Files:    []FileDigest{{FileIndex: 0, SHA256: digestFor(content)}},
	})
	if code := errorCode(t, err); code != "upload_content_type_mismatch" {
		t.Fatalf("code = %q", code)
	}
}

func TestUploadSessionFinishesTypedPrivateBatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	manager := newTestManager(t, &now)
	target := uploadTarget(4)
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("bounded image")...)
	markdown := []byte("# inert markdown\n<script>not executed</script>\n")
	begin, err := manager.Begin(BeginRequest{Target: target, Files: []FileSpec{
		{Name: "screen.png", MediaType: "image/png", Bytes: int64(len(png))},
		{Name: "result.md", MediaType: "text/markdown", Bytes: int64(len(markdown))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sendChunk(t, manager, target, begin.UploadID, 0, 0, png)
	sendChunk(t, manager, target, begin.UploadID, 1, 1, markdown)
	finished, err := manager.Finish(FinishRequest{Target: target, UploadID: begin.UploadID, Files: []FileDigest{
		{FileIndex: 0, SHA256: digestFor(png)}, {FileIndex: 1, SHA256: digestFor(markdown)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(finished.Attachments) != 2 || finished.Attachments[0].Ref == finished.Attachments[1].Ref {
		t.Fatalf("attachments = %#v", finished.Attachments)
	}
	for _, attachment := range finished.Attachments {
		if attachment.Path == "" || strings.Contains(filepath.Base(attachment.Path), attachment.Name) {
			t.Fatalf("attachment path exposes client name: %#v", attachment)
		}
		info, statErr := os.Stat(attachment.Path)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("staged file mode: info=%v err=%v", info, statErr)
		}
		resolved, resolveErr := manager.Resolve(target, attachment.Ref)
		if resolveErr != nil || resolved.SHA256 != attachment.SHA256 {
			t.Fatalf("resolve = %#v, %v", resolved, resolveErr)
		}
	}
}

func TestUploadRejectsTraversalTypesAndBoundsBeforeStaging(t *testing.T) {
	now := time.Now()
	manager := newTestManager(t, &now)
	target := uploadTarget(1)
	cases := []struct {
		name string
		spec FileSpec
		code string
	}{
		{"traversal", FileSpec{Name: "../escape.png", MediaType: "image/png", Bytes: 8}, "upload_name_invalid"},
		{"absolute", FileSpec{Name: "/tmp/escape.png", MediaType: "image/png", Bytes: 8}, "upload_name_invalid"},
		{"extension spoof", FileSpec{Name: "payload.exe", MediaType: "image/png", Bytes: 8}, "upload_extension_mismatch"},
		{"mime spoof", FileSpec{Name: "payload.png", MediaType: "application/octet-stream", Bytes: 8}, "upload_type_unsupported"},
		{"oversize", FileSpec{Name: "large.png", MediaType: "image/png", Bytes: 4097}, "upload_file_size_invalid"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.Begin(BeginRequest{Target: target, Files: []FileSpec{test.spec}})
			if code := errorCode(t, err); code != test.code {
				t.Fatalf("code = %q, want %q", code, test.code)
			}
		})
	}
	_, err := manager.Begin(BeginRequest{Target: target, Files: []FileSpec{
		{Name: "a.png", MediaType: "image/png", Bytes: 3000},
		{Name: "b.png", MediaType: "image/png", Bytes: 3000},
		{Name: "c.png", MediaType: "image/png", Bytes: 3000},
	}})
	if code := errorCode(t, err); code != "upload_batch_too_large" {
		t.Fatalf("batch code = %q", code)
	}
}

func TestChunkFailuresDiscardPartialSession(t *testing.T) {
	now := time.Now()
	target := uploadTarget(2)
	for _, test := range []struct {
		name    string
		request func(BeginResult) ChunkRequest
		code    string
	}{
		{"out of order", func(begin BeginResult) ChunkRequest {
			data := []byte("x")
			return ChunkRequest{Target: target, UploadID: begin.UploadID, FileIndex: 0, Sequence: 1, Data: data, SHA256: digestFor(data)}
		}, "upload_chunk_out_of_order"},
		{"chunk digest", func(begin BeginResult) ChunkRequest {
			return ChunkRequest{Target: target, UploadID: begin.UploadID, FileIndex: 0, Sequence: 0, Data: []byte("x"), SHA256: strings.Repeat("0", 64)}
		}, "upload_chunk_digest_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newTestManager(t, &now)
			begin := beginOne(t, manager, target, "note.txt", "text/plain", 1)
			_, err := manager.Chunk(test.request(begin))
			if code := errorCode(t, err); code != test.code {
				t.Fatalf("code = %q", code)
			}
			if _, err := manager.Chunk(test.request(begin)); errorCode(t, err) != "upload_session_not_found" {
				t.Fatalf("discarded session error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(manager.rootPath, "sessions", begin.UploadID)); !os.IsNotExist(err) {
				t.Fatalf("partial session remains: %v", err)
			}
		})
	}
}

func TestChunkAfterFinalDeclaredFileIsRejected(t *testing.T) {
	now := time.Now()
	manager := newTestManager(t, &now)
	target := uploadTarget(20)
	begin := beginOne(t, manager, target, "note.txt", "text/plain", 1)
	sendChunk(t, manager, target, begin.UploadID, 0, 0, []byte("x"))

	data := []byte("y")
	_, err := manager.Chunk(ChunkRequest{
		Target: target, UploadID: begin.UploadID, FileIndex: 1, Sequence: 1,
		Data: data, SHA256: digestFor(data),
	})
	if code := errorCode(t, err); code != "upload_chunk_out_of_order" {
		t.Fatalf("code = %q", code)
	}
}

func TestFinishRejectsFinalDigestAndMagicSpoofThenCleans(t *testing.T) {
	now := time.Now()
	target := uploadTarget(3)
	for _, test := range []struct {
		name   string
		data   []byte
		digest string
		code   string
	}{
		{"final digest", []byte("plain text"), strings.Repeat("0", 64), "upload_final_digest_mismatch"},
		{"magic spoof", []byte("not a png"), digestFor([]byte("not a png")), "upload_content_type_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newTestManager(t, &now)
			mediaType, name := "text/plain", "note.txt"
			if test.name == "magic spoof" {
				mediaType, name = "image/png", "image.png"
			}
			begin := beginOne(t, manager, target, name, mediaType, int64(len(test.data)))
			sendChunk(t, manager, target, begin.UploadID, 0, 0, test.data)
			_, err := manager.Finish(FinishRequest{Target: target, UploadID: begin.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: test.digest}}})
			if code := errorCode(t, err); code != test.code {
				t.Fatalf("code = %q", code)
			}
			if _, err := os.Stat(filepath.Join(manager.rootPath, "sessions", begin.UploadID)); !os.IsNotExist(err) {
				t.Fatalf("partial remains: %v", err)
			}
		})
	}
}

func TestFinishDetectsStagingSymlinkReplacementWithoutTouchingTarget(t *testing.T) {
	now := time.Now()
	manager := newTestManager(t, &now)
	target := uploadTarget(5)
	data := []byte("safe text")
	begin := beginOne(t, manager, target, "safe.txt", "text/plain", int64(len(data)))
	sendChunk(t, manager, target, begin.UploadID, 0, 0, data)
	partial := filepath.Join(manager.rootPath, "sessions", begin.UploadID, "0000.part")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(partial); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, partial); err != nil {
		t.Fatal(err)
	}
	_, err := manager.Finish(FinishRequest{Target: target, UploadID: begin.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(data)}}})
	if code := errorCode(t, err); code != "upload_staging_changed" {
		t.Fatalf("code = %q", code)
	}
	outsideData, readErr := os.ReadFile(outside)
	if readErr != nil || string(outsideData) != "outside" {
		t.Fatalf("outside target changed: %q, %v", outsideData, readErr)
	}
}

func TestUploadScopesExpiryCancelAndCleanup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	manager := newTestManager(t, &now)
	target := uploadTarget(6)
	other := uploadTarget(7)
	begin := beginOne(t, manager, target, "note.txt", "text/plain", 1)
	if code := errorCode(t, manager.Cancel(other, begin.UploadID)); code != "upload_scope_mismatch" {
		t.Fatalf("scope code = %q", code)
	}
	if err := manager.Cancel(target, begin.UploadID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Cancel(target, begin.UploadID); err != nil {
		t.Fatalf("repeated cancel: %v", err)
	}

	expiring := beginOne(t, manager, target, "later.txt", "text/plain", 1)
	now = now.Add(2 * time.Minute)
	if _, err := manager.Chunk(ChunkRequest{Target: target, UploadID: expiring.UploadID, FileIndex: 0, Sequence: 0, Data: []byte("x"), SHA256: digestFor([]byte("x"))}); errorCode(t, err) != "upload_session_not_found" {
		t.Fatalf("expired session error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.rootPath, "sessions", expiring.UploadID)); !os.IsNotExist(err) {
		t.Fatalf("expired partial remains: %v", err)
	}
}

func TestTextValidationHandlesUTF8AcrossChunksAndRejectsInvalidBytes(t *testing.T) {
	now := time.Now()
	target := uploadTarget(8)
	valid := []byte("a€b")
	manager := newTestManager(t, &now)
	begin := beginOne(t, manager, target, "utf8.txt", "text/plain", int64(len(valid)))
	sendChunk(t, manager, target, begin.UploadID, 0, 0, valid[:2])
	sendChunk(t, manager, target, begin.UploadID, 0, 1, valid[2:])
	if _, err := manager.Finish(FinishRequest{Target: target, UploadID: begin.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(valid)}}}); err != nil {
		t.Fatalf("split UTF-8 rejected: %v", err)
	}

	invalid := []byte{0xff, 0xfe}
	manager2 := newTestManager(t, &now)
	begin2 := beginOne(t, manager2, target, "invalid.md", "text/markdown", int64(len(invalid)))
	sendChunk(t, manager2, target, begin2.UploadID, 0, 0, invalid)
	_, err := manager2.Finish(FinishRequest{Target: target, UploadID: begin2.UploadID, Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(invalid)}}})
	if code := errorCode(t, err); code != "upload_content_type_mismatch" {
		t.Fatalf("invalid UTF-8 code = %q", code)
	}
}

func TestFinishedAttachmentsSurviveManagerRestartWithStableScope(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	root := t.TempDir()
	config := Config{
		Root: root, ChunkBytes: 1024, MaxFiles: 3, MaxFileBytes: 4096,
		MaxBatchBytes: 8192, SessionTTL: time.Minute, AttachmentTTL: time.Hour,
		Now: func() time.Time { return now },
	}
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	target := uploadTarget(9)
	data := []byte("persisted attachment")
	begin := beginOne(t, manager, target, "note.txt", "text/plain", int64(len(data)))
	sendChunk(t, manager, target, begin.UploadID, 0, 0, data)
	finished, err := manager.Finish(FinishRequest{
		Target: target, UploadID: begin.UploadID,
		Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(data)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	resolved, err := restarted.Resolve(uploadTarget(10), finished.Attachments[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Name != "note.txt" || resolved.SHA256 != digestFor(data) {
		t.Fatalf("restarted attachment = %#v", resolved)
	}
	otherTarget := uploadTarget(10)
	otherTarget.TerminalID = "other-terminal"
	if _, err := restarted.Resolve(otherTarget, finished.Attachments[0].Ref); errorCode(t, err) != "attachment_scope_mismatch" {
		t.Fatalf("cross-target resolve error = %v", err)
	}
}

func TestInvalidAttachmentIndexIsQuarantinedWithoutDisablingUploads(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	root := t.TempDir()
	legacy := []byte(`{"schema_version":1,"attachments":[{"attachment":{},"target":{"server_session_id":"session","pane_id":"pane","terminal_id":"terminal","generation":7},"rel_path":"objects/stale"}]}`)
	if err := os.WriteFile(filepath.Join(root, attachmentIndexFilename), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Root: root, ChunkBytes: 1024, MaxFiles: 1, MaxFileBytes: 4096,
		MaxBatchBytes: 4096, SessionTTL: time.Minute, AttachmentTTL: time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewManager() with invalid index: %v", err)
	}
	defer manager.Close()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var quarantined string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "attachments.invalid-") {
			quarantined = entry.Name()
			break
		}
	}
	if quarantined == "" {
		t.Fatal("invalid attachment index was not quarantined")
	}
	if got, err := os.ReadFile(filepath.Join(root, quarantined)); err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("quarantined index = %q, %v", got, err)
	}
	if _, err := manager.Begin(BeginRequest{
		Target: uploadTarget(8),
		Files:  []FileSpec{{Name: "after-recovery.txt", MediaType: "text/plain", Bytes: 1}},
	}); err != nil {
		t.Fatalf("upload after index quarantine: %v", err)
	}
}

func TestOneUnusableAttachmentRecordKeepsTheRest(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	root := t.TempDir()
	config := Config{
		Root: root, ChunkBytes: 1024, MaxFiles: 3, MaxFileBytes: 4096,
		MaxBatchBytes: 8192, SessionTTL: time.Minute, AttachmentTTL: time.Hour,
		Now: func() time.Time { return now },
	}
	manager, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	target := uploadTarget(9)
	refs := make([]string, 0, 2)
	for _, name := range []string{"kept.txt", "broken.txt"} {
		data := []byte("attachment " + name)
		begin := beginOne(t, manager, target, name, "text/plain", int64(len(data)))
		sendChunk(t, manager, target, begin.UploadID, 0, 0, data)
		finished, finishErr := manager.Finish(FinishRequest{
			Target: target, UploadID: begin.UploadID,
			Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(data)}},
		})
		if finishErr != nil {
			t.Fatal(finishErr)
		}
		refs = append(refs, finished.Attachments[0].Ref)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt exactly one record's digest. The file still parses and carries
	// the current schema, so only that row is unusable.
	indexPath := filepath.Join(root, attachmentIndexFilename)
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var index attachmentIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	broken := 0
	for position, record := range index.Attachments {
		if record.Attachment.Ref == refs[1] {
			index.Attachments[position].Attachment.SHA256 = "not-a-digest"
			broken++
		}
	}
	if broken != 1 {
		t.Fatalf("corrupted %d records, want 1", broken)
	}
	rewritten, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewManager(config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.Resolve(target, refs[0]); err != nil {
		t.Fatalf("valid attachment lost to a broken sibling: %v", err)
	}
	if _, err := restarted.Resolve(target, refs[1]); errorCode(t, err) != "attachment_not_found" {
		t.Fatalf("broken attachment resolve error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "attachments.invalid-") {
			t.Fatal("one bad record quarantined the whole index")
		}
	}
}

func TestBeginCapsSessionsPerOwnerAndGlobally(t *testing.T) {
	now := time.Now()
	manager, err := NewManager(Config{
		Root: t.TempDir(), ChunkBytes: 1024, MaxFiles: 1, MaxFileBytes: 4096,
		MaxBatchBytes: 4096, MaxSessions: 2, MaxSessionsPerOwner: 1,
		SessionTTL: time.Minute, AttachmentTTL: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	request := func(owner string, generation int64) BeginRequest {
		return BeginRequest{
			Target: uploadTarget(generation),
			Files:  []FileSpec{{Name: "note.txt", MediaType: "text/plain", Bytes: 1}},
			Owner:  owner,
		}
	}
	if _, err := manager.Begin(request("device-a", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Begin(request("device-a", 2)); errorCode(t, err) != "upload_session_limit" {
		t.Fatalf("same-owner cap error = %v", err)
	}
	if _, err := manager.Begin(request("device-b", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Begin(request("device-c", 3)); errorCode(t, err) != "upload_session_limit" {
		t.Fatalf("global cap error = %v", err)
	}
}

func TestFinishRollbackDoesNotPublishPartialRecords(t *testing.T) {
	now := time.Now()
	manager, err := NewManager(Config{
		Root: t.TempDir(), ChunkBytes: 1024, MaxFiles: 2, MaxFileBytes: 4096,
		MaxBatchBytes: 8192, SessionTTL: time.Minute, AttachmentTTL: time.Hour,
		Now: func() time.Time { return now }, Random: bytes.NewReader(make([]byte, 48)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	target := uploadTarget(1)
	data := []byte("x")
	begin, err := manager.Begin(BeginRequest{Target: target, Files: []FileSpec{
		{Name: "a.txt", MediaType: "text/plain", Bytes: 1},
		{Name: "b.txt", MediaType: "text/plain", Bytes: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sendChunk(t, manager, target, begin.UploadID, 0, 0, data)
	sendChunk(t, manager, target, begin.UploadID, 1, 1, data)
	_, err = manager.Finish(FinishRequest{Target: target, UploadID: begin.UploadID, Files: []FileDigest{
		{FileIndex: 0, SHA256: digestFor(data)},
		{FileIndex: 1, SHA256: digestFor(data)},
	}})
	if errorCode(t, err) != "upload_random_unavailable" {
		t.Fatalf("finish error = %v", err)
	}
	if len(manager.attachments) != 0 {
		t.Fatalf("published attachments after rollback = %d", len(manager.attachments))
	}
	objects, err := os.ReadDir(filepath.Join(manager.rootPath, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("objects after rollback = %v", objects)
	}
}

func TestExactFilenameAndIndexedExpirySurviveCleanup(t *testing.T) {
	now := time.Now()
	manager := newTestManager(t, &now)
	target := uploadTarget(2)
	data := []byte("report")
	begin := beginOne(t, manager, target, " report.txt", "text/plain", int64(len(data)))
	sendChunk(t, manager, target, begin.UploadID, 0, 0, data)
	finished, err := manager.Finish(FinishRequest{
		Target: target, UploadID: begin.UploadID,
		Files: []FileDigest{{FileIndex: 0, SHA256: digestFor(data)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Attachments[0].Name != " report.txt" {
		t.Fatalf("name = %q", finished.Attachments[0].Name)
	}
	path := finished.Attachments[0].Path
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	manager.Cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unexpired indexed object was pruned: %v", err)
	}
}

func TestStartupRemovesNonResumableSessionsAndPrunesLegacyUploads(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions", "stale-session"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions", "stale-session", "0000.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "20260902-123456-deadbeef-photo.jpg")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(legacy, old, old); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Root: root, ChunkBytes: 1024, MaxFiles: 1, MaxFileBytes: 4096,
		MaxBatchBytes: 4096, SessionTTL: time.Minute, AttachmentTTL: time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if _, err := os.Stat(filepath.Join(root, "sessions", "stale-session")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale session still exists: %v", err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy upload still exists: %v", err)
	}
}
