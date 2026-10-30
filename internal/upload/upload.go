package upload

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const (
	DefaultChunkBytes          = 256 * 1024
	DefaultMaxFiles            = 8
	DefaultMaxFileBytes        = 20 * 1024 * 1024
	DefaultMaxBatchBytes       = 50 * 1024 * 1024
	DefaultMaxSessions         = 64
	DefaultMaxSessionsPerOwner = 4
	DefaultSessionTTL          = 30 * time.Minute
	DefaultAttachmentTTL       = 7 * 24 * time.Hour
	attachmentIndexFilename    = "attachments.json"
	attachmentIndexVersion     = 1
)

type attachmentIndexPublishedError struct {
	err error
}

func (e *attachmentIndexPublishedError) Error() string { return e.err.Error() }
func (e *attachmentIndexPublishedError) Unwrap() error { return e.err }

type Error struct {
	Code string         `json:"code"`
	Args map[string]any `json:"args,omitempty"`
}

func (e *Error) Error() string { return e.Code }

func failure(code string, args map[string]any) error {
	return &Error{Code: code, Args: args}
}

type Config struct {
	Root                string
	ChunkBytes          int
	MaxFiles            int
	MaxFileBytes        int64
	MaxBatchBytes       int64
	MaxSessions         int
	MaxSessionsPerOwner int
	SessionTTL          time.Duration
	AttachmentTTL       time.Duration
	Now                 func() time.Time
	Random              io.Reader
	// Logger records index recovery. Nil discards those records.
	Logger *slog.Logger
}

type FileSpec struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
}

type Limits struct {
	MaxFiles      int   `json:"max_files"`
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxBatchBytes int64 `json:"max_batch_bytes"`
}

type BeginRequest struct {
	Target protocol.TargetRef `json:"target"`
	Files  []FileSpec         `json:"files"`
	Owner  string             `json:"-"`
}

type BeginResult struct {
	UploadID   string    `json:"upload_id"`
	ChunkBytes int       `json:"chunk_bytes"`
	ExpiresAt  time.Time `json:"expires_at"`
	Limits     Limits    `json:"limits"`
}

type ChunkRequest struct {
	Target    protocol.TargetRef `json:"target"`
	UploadID  string             `json:"upload_id"`
	FileIndex int                `json:"file_index"`
	Sequence  int                `json:"sequence"`
	Data      []byte             `json:"data"`
	SHA256    string             `json:"sha256"`
}

type ChunkResult struct {
	FileIndex     int   `json:"file_index"`
	NextSequence  int   `json:"next_sequence"`
	ReceivedBytes int64 `json:"received_bytes"`
}

type FileDigest struct {
	FileIndex int    `json:"file_index"`
	SHA256    string `json:"sha256"`
}

type FinishRequest struct {
	Target   protocol.TargetRef `json:"target"`
	UploadID string             `json:"upload_id"`
	Files    []FileDigest       `json:"files"`
}

type Attachment struct {
	Ref       string    `json:"ref"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256"`
	ExpiresAt time.Time `json:"expires_at"`
	Path      string    `json:"-"`
}

type FinishResult struct {
	Attachments []Attachment `json:"attachments"`
}

type Manager struct {
	mu                  sync.Mutex
	attachmentMu        sync.Mutex
	randomMu            sync.Mutex
	rootPath            string
	root                *os.Root
	chunkBytes          int
	maxFiles            int
	maxFile             int64
	maxBatch            int64
	maxSessions         int
	maxSessionsPerOwner int
	sessionTTL          time.Duration
	attachTTL           time.Duration
	now                 func() time.Time
	random              io.Reader
	sessions            map[string]*session
	attachments         map[string]*attachmentRecord
	tombstones          map[string]tombstone
}

type session struct {
	mu        sync.Mutex
	id        string
	target    protocol.TargetRef
	owner     string
	expiresAt time.Time
	files     []*sessionFile
	current   int
	sequence  int
}

type sessionFile struct {
	spec       FileSpec
	path       string
	file       *os.File
	received   int64
	hash       hash.Hash
	prefix     []byte
	suffix     []byte
	utf8Tail   []byte
	invalidUTF bool
	hasNUL     bool
}

type attachmentRecord struct {
	attachment     Attachment
	target         protocol.TargetRef
	relPath        string
	persistedScope bool
}
type diskAttachmentTarget struct {
	ServerSessionID string `json:"server_session_id"`
	PaneID          string `json:"pane_id"`
	TerminalID      string `json:"terminal_id"`
	AgentSessionID  string `json:"agent_session_id"`
}
type diskAttachmentRecord struct {
	Attachment Attachment           `json:"attachment"`
	Target     diskAttachmentTarget `json:"target"`
	RelPath    string               `json:"rel_path"`
}

type attachmentIndex struct {
	SchemaVersion int                    `json:"schema_version"`
	Attachments   []diskAttachmentRecord `json:"attachments"`
}

type tombstone struct {
	target    protocol.TargetRef
	expiresAt time.Time
}

func NewManager(config Config) (*Manager, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, failure("upload_root_unavailable", nil)
	}
	if config.ChunkBytes == 0 {
		config.ChunkBytes = DefaultChunkBytes
	}
	if config.MaxFiles == 0 {
		config.MaxFiles = DefaultMaxFiles
	}
	if config.MaxFileBytes == 0 {
		config.MaxFileBytes = DefaultMaxFileBytes
	}
	if config.MaxBatchBytes == 0 {
		config.MaxBatchBytes = DefaultMaxBatchBytes
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = DefaultMaxSessions
	}
	if config.MaxSessionsPerOwner == 0 {
		config.MaxSessionsPerOwner = DefaultMaxSessionsPerOwner
	}
	if config.SessionTTL == 0 {
		config.SessionTTL = DefaultSessionTTL
	}
	if config.AttachmentTTL == 0 {
		config.AttachmentTTL = DefaultAttachmentTTL
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.DiscardHandler)
	}
	if config.ChunkBytes < 1024 || config.ChunkBytes > 1024*1024 || config.MaxFiles < 1 ||
		config.MaxFileBytes < 1 || config.MaxBatchBytes < config.MaxFileBytes ||
		config.MaxSessions < 1 || config.MaxSessionsPerOwner < 1 ||
		config.MaxSessionsPerOwner > config.MaxSessions ||
		config.SessionTTL <= 0 || config.AttachmentTTL <= 0 {
		return nil, failure("upload_config_invalid", nil)
	}
	cleanRoot := filepath.Clean(config.Root)
	if err := os.MkdirAll(cleanRoot, 0o700); err != nil {
		return nil, failure("upload_root_unavailable", nil)
	}
	info, err := os.Lstat(cleanRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, failure("upload_root_unsafe", nil)
	}
	if err := os.Chmod(cleanRoot, 0o700); err != nil {
		return nil, failure("upload_root_unavailable", nil)
	}
	root, err := os.OpenRoot(cleanRoot)
	if err != nil {
		return nil, failure("upload_root_unavailable", nil)
	}
	manager := &Manager{
		rootPath: cleanRoot, root: root, chunkBytes: config.ChunkBytes,
		maxFiles: config.MaxFiles, maxFile: config.MaxFileBytes, maxBatch: config.MaxBatchBytes,
		maxSessions: config.MaxSessions, maxSessionsPerOwner: config.MaxSessionsPerOwner,
		sessionTTL: config.SessionTTL, attachTTL: config.AttachmentTTL,
		now: config.Now, random: config.Random, sessions: make(map[string]*session),
		attachments: make(map[string]*attachmentRecord), tombstones: make(map[string]tombstone),
	}
	if err := manager.ensurePrivateDirectory("sessions"); err != nil {
		root.Close()
		return nil, err
	}
	if err := manager.ensurePrivateDirectory("objects"); err != nil {
		root.Close()
		return nil, err
	}
	if err := manager.clearSessionDiskLocked(); err != nil {
		root.Close()
		return nil, err
	}
	dropped, loadErr := manager.loadAttachmentsLocked()
	if loadErr != nil {
		manager.attachments = make(map[string]*attachmentRecord)
		if quarantineErr := manager.quarantineAttachmentIndexLocked(); quarantineErr != nil {
			root.Close()
			return nil, quarantineErr
		}
		config.Logger.Warn("quarantined unreadable attachment index",
			"root", cleanRoot, "error", loadErr)
	} else if dropped > 0 {
		config.Logger.Warn("dropped unusable attachment records",
			"root", cleanRoot, "dropped", dropped, "kept", len(manager.attachments))
	}
	manager.Cleanup()
	if err := manager.persistAttachments(); err != nil {
		root.Close()
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, current := range m.sessions {
		sessions = append(sessions, current)
	}
	m.sessions = make(map[string]*session)
	m.mu.Unlock()
	for _, current := range sessions {
		current.mu.Lock()
		m.closeSessionFiles(current)
		current.mu.Unlock()
	}
	m.attachmentMu.Lock()
	defer m.attachmentMu.Unlock()
	return m.root.Close()
}

func (m *Manager) Begin(request BeginRequest) (BeginResult, error) {
	m.Cleanup()
	owner := strings.TrimSpace(request.Owner)
	if owner == "" {
		owner = "unattributed"
	}
	if !validTarget(request.Target) {
		return BeginResult{}, failure("upload_target_invalid", nil)
	}
	if len(request.Files) == 0 || len(request.Files) > m.maxFiles {
		return BeginResult{}, failure("upload_batch_count_invalid", map[string]any{"max_files": m.maxFiles})
	}
	var total int64
	files := make([]*sessionFile, len(request.Files))
	for index, spec := range request.Files {
		normalized, err := normalizeSpec(spec, m.maxFile)
		if err != nil {
			return BeginResult{}, err
		}
		if total > m.maxBatch-normalized.Bytes {
			return BeginResult{}, failure("upload_batch_too_large", map[string]any{"max_bytes": m.maxBatch})
		}
		total += normalized.Bytes
		files[index] = &sessionFile{spec: normalized, hash: sha256.New()}
	}
	id, err := m.opaqueID(24)
	if err != nil {
		return BeginResult{}, err
	}
	expires := m.now().Add(m.sessionTTL)
	current := &session{
		id: id, target: request.Target, owner: owner, expiresAt: expires, files: files,
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	m.mu.Lock()
	if len(m.sessions) >= m.maxSessions {
		m.mu.Unlock()
		return BeginResult{}, failure("upload_session_limit", map[string]any{"max_sessions": m.maxSessions})
	}
	ownerSessions := 0
	for _, active := range m.sessions {
		if active.owner == owner {
			ownerSessions++
		}
	}
	if ownerSessions >= m.maxSessionsPerOwner {
		m.mu.Unlock()
		return BeginResult{}, failure("upload_session_limit", map[string]any{"max_sessions": m.maxSessionsPerOwner})
	}
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return BeginResult{}, failure("upload_staging_failed", nil)
	}
	m.sessions[id] = current
	m.mu.Unlock()

	directory := "sessions/" + id
	if err := m.root.Mkdir(directory, 0o700); err != nil {
		m.discardSession(current, false)
		return BeginResult{}, failure("upload_staging_failed", nil)
	}
	for index, item := range files {
		item.path = fmt.Sprintf("%s/%04d.part", directory, index)
		file, openErr := m.root.OpenFile(item.path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if openErr != nil {
			m.discardSession(current, false)
			return BeginResult{}, failure("upload_staging_failed", nil)
		}
		item.file = file
	}
	return BeginResult{
		UploadID: id, ChunkBytes: m.chunkBytes, ExpiresAt: expires,
		Limits: Limits{MaxFiles: m.maxFiles, MaxFileBytes: m.maxFile, MaxBatchBytes: m.maxBatch},
	}, nil
}

func (m *Manager) Chunk(request ChunkRequest) (ChunkResult, error) {
	current, err := m.lockSession(request.UploadID, request.Target)
	if err != nil {
		return ChunkResult{}, err
	}
	defer current.mu.Unlock()
	fail := func(err error) (ChunkResult, error) {
		m.discardSession(current, true)
		return ChunkResult{}, err
	}
	if current.current >= len(current.files) || request.FileIndex != current.current || request.Sequence != current.sequence {
		return fail(failure("upload_chunk_out_of_order", map[string]any{
			"expected_file_index": current.current, "expected_sequence": current.sequence,
		}))
	}
	if len(request.Data) == 0 || len(request.Data) > m.chunkBytes {
		return fail(failure("upload_chunk_size_invalid", map[string]any{"max_bytes": m.chunkBytes}))
	}
	if !validDigest(request.SHA256) || !digestMatches(request.Data, request.SHA256) {
		return fail(failure("upload_chunk_digest_mismatch", nil))
	}
	item := current.files[current.current]
	if item.received > item.spec.Bytes-int64(len(request.Data)) {
		return fail(failure("upload_file_too_large", map[string]any{"expected_bytes": item.spec.Bytes}))
	}
	if _, err := item.file.Write(request.Data); err != nil {
		return fail(failure("upload_staging_failed", nil))
	}
	_, _ = item.hash.Write(request.Data)
	item.received += int64(len(request.Data))
	item.capture(request.Data)
	current.sequence++
	result := ChunkResult{FileIndex: request.FileIndex, NextSequence: current.sequence, ReceivedBytes: item.received}
	if item.received == item.spec.Bytes {
		current.current++
	}
	return result, nil
}

func (m *Manager) Finish(request FinishRequest) (FinishResult, error) {
	current, err := m.lockSession(request.UploadID, request.Target)
	if err != nil {
		return FinishResult{}, err
	}
	defer current.mu.Unlock()
	fail := func(err error) (FinishResult, error) {
		m.discardSession(current, true)
		return FinishResult{}, err
	}
	if current.current != len(current.files) || len(request.Files) != len(current.files) {
		return fail(failure("upload_incomplete", nil))
	}
	for index, item := range current.files {
		claimed := request.Files[index]
		actual := hex.EncodeToString(item.hash.Sum(nil))
		if claimed.FileIndex != index || !validDigest(claimed.SHA256) ||
			subtle.ConstantTimeCompare([]byte(actual), []byte(strings.ToLower(claimed.SHA256))) != 1 {
			return fail(failure("upload_final_digest_mismatch", map[string]any{"file_index": index}))
		}
		if item.received != item.spec.Bytes {
			return fail(failure("upload_incomplete", map[string]any{"file_index": index}))
		}
		if err := validateContent(item); err != nil {
			return fail(err)
		}
		if err := item.file.Sync(); err != nil {
			return fail(failure("upload_staging_failed", nil))
		}
		pathInfo, statErr := m.root.Lstat(item.path)
		openedInfo, openedErr := item.file.Stat()
		if statErr != nil || openedErr != nil || pathInfo.Mode()&os.ModeSymlink != 0 ||
			!pathInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) || openedInfo.Size() != item.spec.Bytes {
			return fail(failure("upload_staging_changed", map[string]any{"file_index": index}))
		}
	}

	m.attachmentMu.Lock()
	defer m.attachmentMu.Unlock()
	expires := m.now().Add(m.attachTTL)
	result := FinishResult{Attachments: make([]Attachment, 0, len(current.files))}
	created := make([]string, 0, len(current.files))
	pending := make([]*attachmentRecord, 0, len(current.files))
	for _, item := range current.files {
		ref, idErr := m.opaqueID(24)
		if idErr != nil {
			for _, path := range created {
				_ = m.root.Remove(path)
			}
			return fail(idErr)
		}
		extension := canonicalExtension(item.spec.MediaType)
		relPath := "objects/" + ref + extension
		if closeErr := item.file.Close(); closeErr != nil {
			item.file = nil
			for _, path := range created {
				_ = m.root.Remove(path)
			}
			return fail(failure("upload_staging_failed", nil))
		}
		item.file = nil
		if renameErr := m.root.Rename(item.path, relPath); renameErr != nil {
			for _, path := range created {
				_ = m.root.Remove(path)
			}
			return fail(failure("upload_staging_failed", nil))
		}
		created = append(created, relPath)
		digest := hex.EncodeToString(item.hash.Sum(nil))
		attachment := Attachment{
			Ref: ref, Name: item.spec.Name, MediaType: item.spec.MediaType, Bytes: item.spec.Bytes,
			SHA256: digest, ExpiresAt: expires, Path: filepath.Join(m.rootPath, filepath.FromSlash(relPath)),
		}
		pending = append(pending, &attachmentRecord{attachment: attachment, target: current.target, relPath: relPath})
		result.Attachments = append(result.Attachments, attachment)
	}
	if syncErr := m.syncObjectsDirectory(); syncErr != nil {
		for _, path := range created {
			_ = m.root.Remove(path)
		}
		return fail(syncErr)
	}
	m.mu.Lock()
	collision := false
	for _, record := range pending {
		if _, exists := m.attachments[record.attachment.Ref]; exists {
			collision = true
			break
		}
	}
	if !collision {
		for _, record := range pending {
			m.attachments[record.attachment.Ref] = record
		}
	}
	m.mu.Unlock()
	if collision {
		for _, path := range created {
			_ = m.root.Remove(path)
		}
		return fail(failure("upload_staging_failed", nil))
	}
	if persistErr := m.persistAttachments(); persistErr != nil {
		var published *attachmentIndexPublishedError
		if errors.As(persistErr, &published) {
			return fail(persistErr)
		}
		m.mu.Lock()
		for _, record := range pending {
			delete(m.attachments, record.attachment.Ref)
		}
		m.mu.Unlock()
		for _, path := range created {
			_ = m.root.Remove(path)
		}
		return fail(persistErr)
	}
	m.discardSession(current, true)
	return result, nil
}

func (m *Manager) Cancel(target protocol.TargetRef, uploadID string) error {
	m.mu.Lock()
	current := m.sessions[uploadID]
	if current == nil {
		previous, exists := m.tombstones[uploadID]
		if exists && !m.now().Before(previous.expiresAt) {
			delete(m.tombstones, uploadID)
			exists = false
		}
		m.mu.Unlock()
		if exists && previous.target != target {
			return failure("upload_scope_mismatch", nil)
		}
		return nil
	}
	m.mu.Unlock()
	current.mu.Lock()
	defer current.mu.Unlock()
	m.mu.Lock()
	active := m.sessions[uploadID] == current
	m.mu.Unlock()
	if !active {
		return nil
	}
	if current.target != target {
		return failure("upload_scope_mismatch", nil)
	}
	m.discardSession(current, true)
	return nil
}

func (m *Manager) Resolve(target protocol.TargetRef, ref string) (Attachment, error) {
	m.Cleanup()
	m.attachmentMu.Lock()
	defer m.attachmentMu.Unlock()
	m.mu.Lock()
	record, ok := m.attachments[ref]
	m.mu.Unlock()
	if !ok {
		return Attachment{}, failure("attachment_not_found", nil)
	}
	if !attachmentTargetMatches(record, target) {
		return Attachment{}, failure("attachment_scope_mismatch", nil)
	}
	file, info, err := m.openAttachmentLocked(record)
	if err != nil {
		m.removeAttachment(record)
		if persistErr := m.persistAttachments(); persistErr != nil {
			return Attachment{}, persistErr
		}
		return Attachment{}, err
	}
	_ = file.Close()
	if info.Size() != record.attachment.Bytes {
		m.removeAttachment(record)
		if persistErr := m.persistAttachments(); persistErr != nil {
			return Attachment{}, persistErr
		}
		return Attachment{}, failure("attachment_changed", nil)
	}
	return record.attachment, nil
}

func (m *Manager) Cleanup() int {
	now := m.now()
	m.mu.Lock()
	expiredSessions := make([]*session, 0)
	for id, current := range m.sessions {
		if !now.Before(current.expiresAt) {
			delete(m.sessions, id)
			expiredSessions = append(expiredSessions, current)
		}
	}
	for id, previous := range m.tombstones {
		if !now.Before(previous.expiresAt) {
			delete(m.tombstones, id)
		}
	}
	m.mu.Unlock()
	for _, current := range expiredSessions {
		current.mu.Lock()
		m.closeSessionFiles(current)
		_ = m.root.RemoveAll("sessions/" + current.id)
		current.mu.Unlock()
	}

	m.attachmentMu.Lock()
	expiredAttachments := make([]*attachmentRecord, 0)
	m.mu.Lock()
	for ref, record := range m.attachments {
		if !now.Before(record.attachment.ExpiresAt) {
			delete(m.attachments, ref)
			expiredAttachments = append(expiredAttachments, record)
		}
	}
	m.mu.Unlock()
	for _, record := range expiredAttachments {
		_ = m.root.Remove(record.relPath)
	}
	diskRemoved := m.cleanupDiskLocked()
	if len(expiredAttachments) > 0 {
		_ = m.persistAttachments()
	}
	m.attachmentMu.Unlock()
	return len(expiredSessions) + len(expiredAttachments) + diskRemoved
}

func (m *Manager) lockSession(id string, target protocol.TargetRef) (*session, error) {
	m.mu.Lock()
	current := m.sessions[id]
	m.mu.Unlock()
	if current == nil {
		return nil, failure("upload_session_not_found", nil)
	}
	current.mu.Lock()
	m.mu.Lock()
	active := m.sessions[id] == current
	if active && !m.now().Before(current.expiresAt) {
		delete(m.sessions, id)
		active = false
	}
	m.mu.Unlock()
	if !active {
		m.closeSessionFiles(current)
		_ = m.root.RemoveAll("sessions/" + current.id)
		current.mu.Unlock()
		return nil, failure("upload_session_not_found", nil)
	}
	if current.target != target {
		current.mu.Unlock()
		return nil, failure("upload_scope_mismatch", nil)
	}
	return current, nil
}

// discardSession removes a session from shared state before doing filesystem
// cleanup. The caller owns current.mu, so no chunk can write while files close.
func (m *Manager) discardSession(current *session, tombstoneSession bool) {
	m.mu.Lock()
	if m.sessions[current.id] == current {
		delete(m.sessions, current.id)
		if tombstoneSession {
			m.tombstones[current.id] = tombstone{target: current.target, expiresAt: current.expiresAt}
		}
	}
	m.mu.Unlock()
	m.closeSessionFiles(current)
	_ = m.root.RemoveAll("sessions/" + current.id)
}

func (m *Manager) removeAttachment(record *attachmentRecord) {
	m.mu.Lock()
	if m.attachments[record.attachment.Ref] == record {
		delete(m.attachments, record.attachment.Ref)
	}
	m.mu.Unlock()
	_ = m.root.Remove(record.relPath)
}

func (m *Manager) closeSessionFiles(current *session) {
	for _, item := range current.files {
		if item.file != nil {
			_ = item.file.Close()
			item.file = nil
		}
	}
}

func (m *Manager) cleanupDiskLocked() int {
	now := m.now()
	removed := 0
	indexedObjects := make(map[string]struct{}, len(m.attachments))
	for _, record := range m.attachments {
		indexedObjects[record.relPath] = struct{}{}
	}
	for _, directory := range []struct {
		name string
		ttl  time.Duration
	}{{"sessions", m.sessionTTL}, {"objects", m.attachTTL}} {
		entries, err := fs.ReadDir(m.root.FS(), directory.name)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			path := directory.name + "/" + entry.Name()
			info, infoErr := entry.Info()
			if infoErr != nil {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				_ = m.root.Remove(path)
				removed++
				continue
			}
			if directory.name == "objects" {
				if _, indexed := indexedObjects[path]; indexed {
					continue
				}
			}
			if now.Sub(info.ModTime()) < directory.ttl {
				continue
			}
			if directory.name == "sessions" && entry.IsDir() {
				_ = m.root.RemoveAll(path)
				removed++
			} else if directory.name == "objects" && info.Mode().IsRegular() {
				_ = m.root.Remove(path)
				removed++
			}
		}
	}
	entries, err := fs.ReadDir(m.root.FS(), ".")
	if err == nil {
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !legacyUploadFilename.MatchString(entry.Name()) {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr == nil && now.Sub(info.ModTime()) >= m.attachTTL {
				_ = m.root.Remove(entry.Name())
				removed++
			}
		}
	}
	return removed
}

// loadAttachmentsLocked restores the persisted attachment index and reports how
// many records it had to drop. A record that no longer describes a usable
// object is dropped alone: quarantining the file over one bad row would strand
// every other reference a phone still holds. Dropped objects are left for the
// TTL sweep in cleanupDiskLocked, which removes unindexed objects — a corrupt
// row's path is not trustworthy enough to delete from here. Only a file that
// cannot be parsed, or that carries another schema, is reported as an error for
// the caller to quarantine.
func (m *Manager) loadAttachmentsLocked() (int, error) {
	info, err := m.root.Lstat(attachmentIndexFilename)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, failure("upload_metadata_invalid", nil)
	}
	file, err := m.root.Open(attachmentIndexFilename)
	if err != nil {
		return 0, failure("upload_metadata_invalid", nil)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	var index attachmentIndex
	if err := decoder.Decode(&index); err != nil || index.SchemaVersion != attachmentIndexVersion {
		return 0, failure("upload_metadata_invalid", nil)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return 0, failure("upload_metadata_invalid", nil)
	}
	now := m.now()
	dropped := 0
	for _, stored := range index.Attachments {
		attachment := stored.Attachment
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(attachment.Ref)
		_, specErr := normalizeSpec(FileSpec{Name: attachment.Name, MediaType: attachment.MediaType, Bytes: attachment.Bytes}, m.maxFile)
		expectedPath := "objects/" + attachment.Ref + canonicalExtension(attachment.MediaType)
		if decodeErr != nil || len(decoded) != 24 || specErr != nil || !validDigest(attachment.SHA256) ||
			!validDiskTarget(stored.Target) || stored.RelPath != expectedPath || attachment.ExpiresAt.IsZero() {
			dropped++
			continue
		}
		if !now.Before(attachment.ExpiresAt) {
			_ = m.root.Remove(stored.RelPath)
			continue
		}
		if _, exists := m.attachments[attachment.Ref]; exists {
			// The first record already owns this object; the duplicate is the
			// only thing discarded, never the file both rows point at.
			dropped++
			continue
		}
		attachment.Path = filepath.Join(m.rootPath, filepath.FromSlash(stored.RelPath))
		record := &attachmentRecord{
			attachment:     attachment,
			target:         stored.Target.protocolTarget(),
			relPath:        stored.RelPath,
			persistedScope: true,
		}
		opened, openedInfo, openErr := m.openAttachmentLocked(record)
		if openErr != nil || openedInfo.Size() != attachment.Bytes {
			if opened != nil {
				_ = opened.Close()
			}
			dropped++
			continue
		}
		_ = opened.Close()
		m.attachments[attachment.Ref] = record
	}
	return dropped, nil
}

func (m *Manager) quarantineAttachmentIndexLocked() error {
	stamp := m.now().UTC().UnixNano()
	for attempt := range 1000 {
		name := fmt.Sprintf("attachments.invalid-%d.json", stamp)
		if attempt > 0 {
			name = fmt.Sprintf("attachments.invalid-%d-%d.json", stamp, attempt)
		}
		if _, err := m.root.Lstat(name); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return failure("upload_metadata_invalid", nil)
		}
		if err := m.root.Rename(attachmentIndexFilename, name); err != nil {
			return failure("upload_metadata_invalid", nil)
		}
		return nil
	}
	return failure("upload_metadata_invalid", nil)
}

func (m *Manager) persistAttachments() error {
	m.mu.Lock()
	records := make([]diskAttachmentRecord, 0, len(m.attachments))
	for _, record := range m.attachments {
		records = append(records, diskAttachmentRecord{
			Attachment: record.attachment,
			Target:     newDiskAttachmentTarget(record.target),
			RelPath:    record.relPath,
		})
	}
	m.mu.Unlock()
	sort.Slice(records, func(left, right int) bool {
		return records[left].Attachment.Ref < records[right].Attachment.Ref
	})
	data, err := json.Marshal(attachmentIndex{SchemaVersion: attachmentIndexVersion, Attachments: records})
	if err != nil {
		return failure("upload_metadata_unavailable", nil)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(m.rootPath, ".attachments-*.tmp")
	if err != nil {
		return failure("upload_metadata_unavailable", nil)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return failure("upload_metadata_unavailable", nil)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return failure("upload_metadata_unavailable", nil)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return failure("upload_metadata_unavailable", nil)
	}
	if err := temp.Close(); err != nil {
		return failure("upload_metadata_unavailable", nil)
	}
	path := filepath.Join(m.rootPath, attachmentIndexFilename)
	if err := os.Rename(tempPath, path); err != nil {
		return failure("upload_metadata_unavailable", nil)
	}
	directory, err := os.Open(m.rootPath)
	if err != nil {
		return &attachmentIndexPublishedError{err: failure("upload_metadata_unavailable", nil)}
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return &attachmentIndexPublishedError{err: failure("upload_metadata_unavailable", nil)}
	}
	return nil
}

func (m *Manager) syncObjectsDirectory() error {
	directory, err := m.root.Open("objects")
	if err != nil {
		return failure("upload_metadata_unavailable", nil)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return failure("upload_metadata_unavailable", nil)
	}
	return nil
}

func (m *Manager) clearSessionDiskLocked() error {
	entries, err := fs.ReadDir(m.root.FS(), "sessions")
	if err != nil {
		return failure("upload_root_unavailable", nil)
	}
	for _, entry := range entries {
		path := "sessions/" + entry.Name()
		if entry.IsDir() {
			if err := m.root.RemoveAll(path); err != nil {
				return failure("upload_root_unavailable", nil)
			}
			continue
		}
		if err := m.root.Remove(path); err != nil {
			return failure("upload_root_unavailable", nil)
		}
	}
	return nil
}

func (m *Manager) ensurePrivateDirectory(path string) error {
	if err := m.root.Mkdir(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return failure("upload_root_unavailable", nil)
	}
	info, err := m.root.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return failure("upload_root_unsafe", nil)
	}
	if err := m.root.Chmod(path, 0o700); err != nil {
		return failure("upload_root_unavailable", nil)
	}
	return nil
}

func (m *Manager) openAttachmentLocked(record *attachmentRecord) (*os.File, fs.FileInfo, error) {
	info, err := m.root.Lstat(record.relPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, failure("attachment_changed", nil)
	}
	file, err := m.root.Open(record.relPath)
	if err != nil {
		return nil, nil, failure("attachment_changed", nil)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil, failure("attachment_changed", nil)
	}
	return file, opened, nil
}

func (m *Manager) opaqueID(bytes int) (string, error) {
	value := make([]byte, bytes)
	m.randomMu.Lock()
	_, err := io.ReadFull(m.random, value)
	m.randomMu.Unlock()
	if err != nil {
		return "", failure("upload_random_unavailable", nil)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func normalizeSpec(spec FileSpec, maxBytes int64) (FileSpec, error) {
	spec.MediaType = strings.ToLower(strings.TrimSpace(strings.Split(spec.MediaType, ";")[0]))
	if strings.TrimSpace(spec.Name) == "" || len(spec.Name) > 128 || !utf8.ValidString(spec.Name) ||
		strings.ContainsAny(spec.Name, "/\\\x00\r\n") || spec.Name == "." || spec.Name == ".." ||
		filepath.Base(spec.Name) != spec.Name {
		return FileSpec{}, failure("upload_name_invalid", nil)
	}
	if spec.Bytes < 1 || spec.Bytes > maxBytes {
		return FileSpec{}, failure("upload_file_size_invalid", map[string]any{"max_bytes": maxBytes})
	}
	allowedExtensions, ok := allowedTypes[spec.MediaType]
	if !ok {
		return FileSpec{}, failure("upload_type_unsupported", nil)
	}
	extension := strings.ToLower(filepath.Ext(spec.Name))
	if !allowedExtensions[extension] {
		return FileSpec{}, failure("upload_extension_mismatch", nil)
	}
	return spec, nil
}

var allowedTypes = map[string]map[string]bool{
	"image/png":        {".png": true},
	"image/jpeg":       {".jpg": true, ".jpeg": true},
	"image/gif":        {".gif": true},
	"image/webp":       {".webp": true},
	"image/heic":       {".heic": true},
	"image/heif":       {".heif": true},
	"application/pdf":  {".pdf": true},
	"application/json": {".json": true},
	"text/csv":         {".csv": true},
	"text/plain":       {".txt": true, ".text": true, ".log": true},
	"text/markdown":    {".md": true, ".markdown": true},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {".docx": true},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {".xlsx": true},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {".pptx": true},
	"application/vnd.oasis.opendocument.text":                                   {".odt": true},
	"application/vnd.oasis.opendocument.spreadsheet":                            {".ods": true},
	"application/vnd.oasis.opendocument.presentation":                           {".odp": true},
}

func canonicalExtension(mediaType string) string {
	extensions := map[string]string{
		"image/png":        ".png",
		"image/jpeg":       ".jpg",
		"image/gif":        ".gif",
		"image/webp":       ".webp",
		"image/heic":       ".heic",
		"image/heif":       ".heif",
		"application/pdf":  ".pdf",
		"application/json": ".json",
		"text/csv":         ".csv",
		"text/plain":       ".txt",
		"text/markdown":    ".md",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
		"application/vnd.oasis.opendocument.text":                                   ".odt",
		"application/vnd.oasis.opendocument.spreadsheet":                            ".ods",
		"application/vnd.oasis.opendocument.presentation":                           ".odp",
	}
	if extension := extensions[mediaType]; extension != "" {
		return extension
	}
	return ".data"
}

var legacyUploadFilename = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-fA-F]{8}-.+[.](png|jpg|jpeg|webp|gif|heic|heif|img)$`)

func newDiskAttachmentTarget(target protocol.TargetRef) diskAttachmentTarget {
	return diskAttachmentTarget{
		ServerSessionID: target.ServerSessionID,
		PaneID:          target.PaneID,
		TerminalID:      target.TerminalID,
		AgentSessionID:  target.AgentSessionID,
	}
}

func (target diskAttachmentTarget) protocolTarget() protocol.TargetRef {
	return protocol.TargetRef{
		ServerSessionID: target.ServerSessionID,
		PaneID:          target.PaneID,
		TerminalID:      target.TerminalID,
		AgentSessionID:  target.AgentSessionID,
	}
}

func validDiskTarget(target diskAttachmentTarget) bool {
	return strings.TrimSpace(target.ServerSessionID) != "" && strings.TrimSpace(target.PaneID) != "" &&
		strings.TrimSpace(target.TerminalID) != ""
}

func attachmentTargetMatches(record *attachmentRecord, target protocol.TargetRef) bool {
	if !record.persistedScope {
		return record.target == target
	}
	return record.target.ServerSessionID == target.ServerSessionID &&
		record.target.PaneID == target.PaneID &&
		record.target.TerminalID == target.TerminalID &&
		record.target.AgentSessionID == target.AgentSessionID
}

func validTarget(target protocol.TargetRef) bool {
	return strings.TrimSpace(target.ServerSessionID) != "" && strings.TrimSpace(target.PaneID) != "" &&
		strings.TrimSpace(target.TerminalID) != "" && target.Generation >= 0
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func digestMatches(data []byte, claimed string) bool {
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	return subtle.ConstantTimeCompare([]byte(actual), []byte(strings.ToLower(claimed))) == 1
}

func (item *sessionFile) capture(data []byte) {
	if len(item.prefix) < 512 {
		remaining := 512 - len(item.prefix)
		if len(data) < remaining {
			remaining = len(data)
		}
		item.prefix = append(item.prefix, data[:remaining]...)
	}
	if len(data) >= 1024 {
		item.suffix = append(item.suffix[:0], data[len(data)-1024:]...)
	} else {
		item.suffix = append(item.suffix, data...)
		if len(item.suffix) > 1024 {
			item.suffix = append(item.suffix[:0], item.suffix[len(item.suffix)-1024:]...)
		}
	}
	if strings.HasPrefix(item.spec.MediaType, "text/") || item.spec.MediaType == "application/json" {
		if bytesContainNUL(data) {
			item.hasNUL = true
		}
		item.captureUTF8(data)
	}
}

func (item *sessionFile) captureUTF8(data []byte) {
	combined := make([]byte, 0, len(item.utf8Tail)+len(data))
	combined = append(combined, item.utf8Tail...)
	combined = append(combined, data...)
	item.utf8Tail = item.utf8Tail[:0]
	for len(combined) > 0 {
		if combined[0] < utf8.RuneSelf {
			combined = combined[1:]
			continue
		}
		if !utf8.FullRune(combined) {
			item.utf8Tail = append(item.utf8Tail, combined...)
			return
		}
		decoded, size := utf8.DecodeRune(combined)
		if decoded == utf8.RuneError && size == 1 {
			item.invalidUTF = true
			return
		}
		combined = combined[size:]
	}
}

func bytesContainNUL(data []byte) bool {
	for _, value := range data {
		if value == 0 {
			return true
		}
	}
	return false
}

func validateContent(item *sessionFile) error {
	prefix := item.prefix
	valid := false
	switch item.spec.MediaType {
	case "image/png":
		valid = len(prefix) >= 8 && string(prefix[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/jpeg":
		valid = len(prefix) >= 3 && prefix[0] == 0xff && prefix[1] == 0xd8 && prefix[2] == 0xff
	case "image/gif":
		valid = len(prefix) >= 6 && (string(prefix[:6]) == "GIF87a" || string(prefix[:6]) == "GIF89a")
	case "image/webp":
		valid = len(prefix) >= 12 && string(prefix[:4]) == "RIFF" && string(prefix[8:12]) == "WEBP"
	case "image/heic", "image/heif":
		valid = validISOImage(prefix)
	case "application/pdf":
		valid = len(prefix) >= 5 && string(prefix[:5]) == "%PDF-" && strings.Contains(string(item.suffix), "%%EOF")
	case "application/json":
		valid = validTextContent(item) && validJSONContent(item.file)
	case "text/plain", "text/markdown", "text/csv":
		valid = validTextContent(item)
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		valid = validDocumentContainer(item.file, item.spec.Bytes, "word/document.xml", "")
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		valid = validDocumentContainer(item.file, item.spec.Bytes, "xl/workbook.xml", "")
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		valid = validDocumentContainer(item.file, item.spec.Bytes, "ppt/presentation.xml", "")
	case "application/vnd.oasis.opendocument.text":
		valid = validDocumentContainer(item.file, item.spec.Bytes, "", "application/vnd.oasis.opendocument.text")
	case "application/vnd.oasis.opendocument.spreadsheet":
		valid = validDocumentContainer(item.file, item.spec.Bytes, "", "application/vnd.oasis.opendocument.spreadsheet")
	case "application/vnd.oasis.opendocument.presentation":
		valid = validDocumentContainer(item.file, item.spec.Bytes, "", "application/vnd.oasis.opendocument.presentation")
	}
	if !valid {
		return failure("upload_content_type_mismatch", map[string]any{"file_index": -1})
	}
	return nil
}

func validTextContent(item *sessionFile) bool {
	return !item.invalidUTF && len(item.utf8Tail) == 0 && !item.hasNUL
}

func validJSONContent(file *os.File) bool {
	if file == nil {
		return false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	decoder := json.NewDecoder(file)
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	var trailing json.RawMessage
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func validDocumentContainer(file *os.File, size int64, requiredPath, requiredMIME string) bool {
	if file == nil || size < 1 {
		return false
	}
	container, err := zip.NewReader(file, size)
	if err != nil || len(container.File) == 0 || len(container.File) > 4096 {
		return false
	}
	foundPath := requiredPath == ""
	foundMIME := requiredMIME == ""
	var expanded uint64
	for _, entry := range container.File {
		name := strings.ReplaceAll(entry.Name, "\\", "/")
		if name == "" || strings.HasPrefix(name, "/") || filepath.Clean(name) != name ||
			name == ".." || strings.HasPrefix(name, "../") || entry.Mode()&os.ModeSymlink != 0 {
			return false
		}
		expanded += entry.UncompressedSize64
		if expanded > 200*1024*1024 {
			return false
		}
		if name == requiredPath {
			foundPath = true
		}
		if name == "mimetype" && requiredMIME != "" {
			if entry.UncompressedSize64 > 128 {
				return false
			}
			reader, openErr := entry.Open()
			if openErr != nil {
				return false
			}
			content, readErr := io.ReadAll(io.LimitReader(reader, 129))
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil || string(content) != requiredMIME {
				return false
			}
			foundMIME = true
		}
	}
	return foundPath && foundMIME
}

func validISOImage(data []byte) bool {
	if len(data) < 16 || string(data[4:8]) != "ftyp" {
		return false
	}
	boxSize := int64(data[0])<<24 | int64(data[1])<<16 | int64(data[2])<<8 | int64(data[3])
	if boxSize < 16 || boxSize > int64(len(data)) {
		return false
	}
	brands := map[string]bool{"heic": true, "heix": true, "hevc": true, "hevx": true, "heim": true, "heis": true, "mif1": true, "msf1": true}
	if brands[string(data[8:12])] {
		return true
	}
	for offset := 16; offset+4 <= int(boxSize); offset += 4 {
		if brands[string(data[offset:offset+4])] {
			return true
		}
	}
	return false
}
