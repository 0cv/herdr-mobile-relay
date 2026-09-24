// Package managedstate implements the private managed-ownership state helper
// for one canonical Herdr config root holding relay.env.
//
// B1 deliberately owns only the filesystem-level owner lock (O), the
// transaction lock (T), and a monotonic generation counter. It contains no
// shell, CLI, service, socket, updater or device-store wiring; later S6 batches
// consume this helper. Positive Serve admission stays fail-closed.
//
// Storage inside the canonical root (nothing else is read or created):
//
//	owner.lock/            directory mode 0700, created with os.Mkdir
//	owner.lock/owner.json  regular file mode 0600, strict bounded record
//	txn.lock/              directory mode 0700, empty while held
//	generation             regular file mode 0600, ASCII decimal uint64
//
// The audited design lists four managed files and states that generation is
// read-modify-written under O exclusivity. Persisting it durably needs its own
// bounded location, so B1 adds exactly the generation file above. The S7
// journal.json and S8 state.json are NOT read or created here.
package managedstate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Sentinel errors. Busy, Timeout, ChangedRoot, ForeignState, InvalidRecord and
// UnknownAuthority never authorize deletion or takeover. RetainedEvidence means
// the lock is occupied by a partial or foreign object and is left in place.
var (
	ErrBusy             = errors.New("managedstate: lock held by another owner or acquisition")
	ErrTimeout          = errors.New("managedstate: bounded transaction contention expired")
	ErrChangedRoot      = errors.New("managedstate: canonical root identity changed")
	ErrForeignState     = errors.New("managedstate: foreign managed state")
	ErrInvalidRecord    = errors.New("managedstate: invalid managed record")
	ErrUnknownAuthority = errors.New("managedstate: unknown managed authority")
	ErrRetainedEvidence = errors.New("managedstate: retained managed evidence")
	ErrRandomFailure    = errors.New("managedstate: random source failure")
	ErrIOFailure        = errors.New("managedstate: filesystem operation failed")
)

const (
	maxRecordBytes = 16 * 1024
	maxJSONDepth   = 32

	ownerLockName  = "owner.lock"
	ownerFileName  = "owner.json"
	txnLockName    = "txn.lock"
	generationName = "generation"

	nonceBytes = 32
)

// Root is an opened canonical root. It retains a directory handle so later
// identity comparisons do not depend on the public pathname alone.
type Root struct {
	path   string
	dev    uint64
	ino    uint64
	handle *os.File
	closed bool
}

// Owner is one acquired-but-not-necessarily-published owner generation.
type Owner struct {
	root       *Root
	nonce      string
	generation uint64
	pid        int
	startedAt  string
	lockDev    uint64
	lockIno    uint64
	recordJSON []byte

	published bool
	closing   bool
	retired   bool
	closed    bool
	txnHeld   bool
}

// Txn is a held transaction lock inside the canonical root.
type Txn struct {
	owner    *Owner
	root     *Root
	path     string
	dev      uint64
	ino      uint64
	released bool
}

// OpenExistingRoot canonicalizes path to its physical directory, requires a
// private 0700 directory, and retains an open handle. It never creates or
// modifies anything.
func OpenExistingRoot(path string) (*Root, error) {
	if path == "" {
		return nil, ErrIOFailure
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, ErrIOFailure
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, ErrIOFailure
	}
	handle, err := os.Open(resolved)
	if err != nil {
		return nil, ErrIOFailure
	}
	handleInfo, err := handle.Stat()
	if err != nil {
		handle.Close()
		return nil, ErrIOFailure
	}
	if !handleInfo.IsDir() || handleInfo.Mode().Perm() != 0o700 {
		handle.Close()
		return nil, ErrIOFailure
	}
	dev, ino := fileIDs(handleInfo)
	return &Root{path: resolved, dev: dev, ino: ino, handle: handle}, nil
}

// Close releases the retained directory handle. It is idempotent.
func (r *Root) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	if r.handle != nil {
		if err := r.handle.Close(); err != nil {
			return ErrIOFailure
		}
	}
	return nil
}

// TryAcquireOwner takes O non-blocking. A complete valid existing record is
// Busy; a partial or malformed lock is RetainedEvidence; any non-directory
// object at owner.lock is ForeignState. Nothing is ever reclaimed.
func (r *Root) TryAcquireOwner() (*Owner, error) {
	if r == nil || r.closed {
		return nil, ErrIOFailure
	}
	lockPath := filepath.Join(r.path, ownerLockName)
	ownerPath := filepath.Join(lockPath, ownerFileName)

	info, err := os.Lstat(lockPath)
	if err == nil {
		return nil, r.existingLockError(info, ownerPath)
	}
	if !os.IsNotExist(err) {
		return nil, ErrIOFailure
	}

	if err := os.Mkdir(lockPath, 0o700); err != nil {
		if !os.IsExist(err) {
			return nil, ErrIOFailure
		}
		info, lerr := os.Lstat(lockPath)
		if lerr != nil {
			return nil, ErrIOFailure
		}
		return nil, r.existingLockError(info, ownerPath)
	}

	// From here the freshly created lock directory is retained on any failure.
	nonce, err := newNonce()
	if err != nil {
		return nil, ErrRandomFailure
	}
	generation, err := nextGeneration(r.path)
	if err != nil {
		return nil, ErrInvalidRecord
	}
	if err := writeFileAtomic(r.path, generationName, []byte(strconv.FormatUint(generation, 10)), 0o600); err != nil {
		return nil, ErrIOFailure
	}
	lockInfo, err := os.Lstat(lockPath)
	if err != nil {
		return nil, ErrIOFailure
	}
	lockDev, lockIno := fileIDs(lockInfo)
	return &Owner{
		root:       r,
		nonce:      nonce,
		generation: generation,
		pid:        os.Getpid(),
		startedAt:  time.Now().UTC().Format(time.RFC3339),
		lockDev:    lockDev,
		lockIno:    lockIno,
	}, nil
}

func (r *Root) existingLockError(info os.FileInfo, ownerPath string) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrForeignState
	}
	if _, _, err := readOwnerRecord(ownerPath); err != nil {
		return ErrRetainedEvidence
	}
	return ErrBusy
}

// PublishRecord writes owner.json atomically for an unpublished owner. It
// refuses to overwrite any existing object at the record path and reports
// FailedPublish as foreign state without removing it. A second publication is
// not silently idempotent.
func (o *Owner) PublishRecord() error {
	if o == nil || o.root == nil || o.closed || o.retired || o.closing {
		return ErrUnknownAuthority
	}
	if o.published {
		return ErrInvalidRecord
	}
	ownerPath := o.ownerPath()
	if _, err := os.Lstat(ownerPath); err == nil {
		// Never overwrite a partial, foreign or symlinked record path.
		return ErrForeignState
	} else if !os.IsNotExist(err) {
		return ErrIOFailure
	}
	data, err := json.Marshal(ownerRecordJSON{
		Schema:     1,
		Nonce:      o.nonce,
		Generation: o.generation,
		RootDevice: o.root.dev,
		RootInode:  o.root.ino,
		PID:        o.pid,
		StartedAt:  o.startedAt,
	})
	if err != nil {
		return ErrIOFailure
	}
	if err := writeFileAtomic(o.lockPath(), ownerFileName, data, 0o600); err != nil {
		return ErrIOFailure
	}
	o.recordJSON = data
	o.published = true
	return nil
}

// Validate re-proves root identity, lock identity and record ownership. It
// still works after BeginClosing; it refuses after Retire or Close.
func (o *Owner) Validate() error {
	if o == nil || o.root == nil || o.closed || o.retired || !o.published {
		return ErrUnknownAuthority
	}
	if err := o.checkRootAndLock(); err != nil {
		return err
	}
	record, _, err := readOwnerRecord(o.ownerPath())
	if err != nil {
		return ErrInvalidRecord
	}
	if record.Nonce != o.nonce || record.Generation != o.generation {
		return ErrForeignState
	}
	return nil
}

// AcquireTransaction takes T non-recursively. Contention is bounded by limit
// (limit <= 0 means a single attempt) and ctx; it never steals or touches
// another owner's lock.
func (o *Owner) AcquireTransaction(ctx context.Context, limit time.Duration) (*Txn, error) {
	if o == nil || o.root == nil || o.closed || o.retired || !o.published || o.closing {
		return nil, ErrUnknownAuthority
	}
	if o.txnHeld {
		return nil, ErrBusy
	}
	if ctx == nil {
		ctx = context.Background()
	}
	const poll = 5 * time.Millisecond
	lockPath := o.txnPath()
	bounded := limit > 0
	deadline := time.Now().Add(limit)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			info, statErr := os.Lstat(lockPath)
			if statErr != nil {
				return nil, ErrIOFailure
			}
			dev, ino := fileIDs(info)
			o.txnHeld = true
			return &Txn{owner: o, root: o.root, path: lockPath, dev: dev, ino: ino}, nil
		}
		if !os.IsExist(err) {
			return nil, ErrIOFailure
		}
		if info, lerr := os.Lstat(lockPath); lerr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, ErrForeignState
			}
		}
		if !bounded || !time.Now().Before(deadline) {
			return nil, ErrTimeout
		}
		select {
		case <-ctx.Done():
			return nil, ErrTimeout
		case <-time.After(poll):
		}
	}
}

// AcquireTransaction acquires T without requiring O. Live reprint and other
// T-only callers validate the active owner separately and read-only.
func (r *Root) AcquireTransaction(ctx context.Context, limit time.Duration) (*Txn, error) {
	if r == nil || r.closed {
		return nil, ErrUnknownAuthority
	}
	if ctx == nil {
		ctx = context.Background()
	}
	const poll = 5 * time.Millisecond
	lockPath := filepath.Join(r.path, txnLockName)
	bounded := limit > 0
	deadline := time.Now().Add(limit)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			info, statErr := os.Lstat(lockPath)
			if statErr != nil {
				return nil, ErrIOFailure
			}
			dev, ino := fileIDs(info)
			return &Txn{root: r, path: lockPath, dev: dev, ino: ino}, nil
		}
		if !os.IsExist(err) {
			return nil, ErrIOFailure
		}
		if info, lerr := os.Lstat(lockPath); lerr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, ErrForeignState
			}
		}
		if !bounded || !time.Now().Before(deadline) {
			return nil, ErrTimeout
		}
		select {
		case <-ctx.Done():
			return nil, ErrTimeout
		case <-time.After(poll):
		}
	}
}

// Release removes T only when its identity matches the acquisition and it is
// empty. A second release is UnknownAuthority. Foreign objects are never
// removed.
func (t *Txn) Release() error {
	if t == nil || t.released {
		return ErrUnknownAuthority
	}
	info, err := os.Lstat(t.path)
	if err != nil {
		return ErrChangedRoot
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrForeignState
	}
	dev, ino := fileIDs(info)
	if dev != t.dev || ino != t.ino {
		return ErrForeignState
	}
	entries, err := os.ReadDir(t.path)
	if err != nil {
		return ErrForeignState
	}
	if len(entries) > 0 {
		return ErrForeignState
	}
	if err := os.Remove(t.path); err != nil {
		return ErrForeignState
	}
	t.released = true
	if t.owner != nil {
		t.owner.txnHeld = false
	}
	return nil
}

// BeginClosing marks a published owner as closing. Afterwards PublishRecord and
// AcquireTransaction refuse with UnknownAuthority while Validate still works.
func (o *Owner) BeginClosing() error {
	if o == nil || o.root == nil || o.closed || o.retired || !o.published {
		return ErrUnknownAuthority
	}
	o.closing = true
	return nil
}

// Retire removes this owner's own record and lock directory after re-proving
// identity. The generation file is retained.
//
// DR-2 documented guarantee: under the cooperating-writer model every Herdr
// writer takes O/T before touching managed state, so the residual
// check-then-remove window is bounded and any observable mismatch refuses with
// ForeignState (or ChangedRoot for the root). Against an arbitrary same-UID
// noncooperating writer, unconditional foreign-pathname preservation is NOT
// achievable with portable primitives, so that stronger promise is not made
// here and DR-2 remains an open product-owner decision.
func (o *Owner) Retire() error {
	if o == nil || o.root == nil || o.closed || o.retired || !o.published || !o.closing {
		return ErrUnknownAuthority
	}
	if err := o.checkRootAndLock(); err != nil {
		return err
	}
	ownerPath := o.ownerPath()
	info, err := os.Lstat(ownerPath)
	if err != nil {
		return ErrForeignState
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || nlink(info) > 1 {
		return ErrForeignState
	}
	if info.Size() > maxRecordBytes {
		return ErrForeignState
	}
	raw, err := os.ReadFile(ownerPath)
	if err != nil {
		return ErrForeignState
	}
	record, err := decodeOwnerRecord(raw)
	if err != nil {
		return ErrForeignState
	}
	if record.Nonce != o.nonce || record.Generation != o.generation || !bytes.Equal(raw, o.recordJSON) {
		return ErrForeignState
	}
	if err := os.Remove(ownerPath); err != nil {
		return ErrForeignState
	}
	entries, err := os.ReadDir(o.lockPath())
	if err != nil {
		return ErrForeignState
	}
	if len(entries) > 0 {
		return ErrForeignState
	}
	if err := os.Remove(o.lockPath()); err != nil {
		return ErrForeignState
	}
	o.retired = true
	return nil
}

// Close releases the root handle and marks the owner closed. It does not remove
// the lock (only Retire does). It is safe after Retire and is idempotent.
func (o *Owner) Close() error {
	if o == nil {
		return nil
	}
	o.closed = true
	if o.root != nil {
		return o.root.Close()
	}
	return nil
}

func (o *Owner) lockPath() string {
	return filepath.Join(o.root.path, ownerLockName)
}

func (o *Owner) ownerPath() string {
	return filepath.Join(o.lockPath(), ownerFileName)
}

func (o *Owner) txnPath() string {
	return filepath.Join(o.root.path, txnLockName)
}

func (o *Owner) checkRootAndLock() error {
	rootInfo, err := os.Stat(o.root.path)
	if err != nil {
		return ErrChangedRoot
	}
	dev, ino := fileIDs(rootInfo)
	if dev != o.root.dev || ino != o.root.ino {
		return ErrChangedRoot
	}
	lockInfo, err := os.Lstat(o.lockPath())
	if err != nil {
		return ErrForeignState
	}
	if lockInfo.Mode()&os.ModeSymlink != 0 || !lockInfo.IsDir() {
		return ErrForeignState
	}
	lockDev, lockIno := fileIDs(lockInfo)
	if lockDev != o.lockDev || lockIno != o.lockIno {
		return ErrForeignState
	}
	return nil
}

// ownerRecordJSON is the exact write schema; field order is stable so the
// published bytes can be compared on retirement.
type ownerRecordJSON struct {
	Schema     int    `json:"schema"`
	Nonce      string `json:"nonce"`
	Generation uint64 `json:"generation"`
	RootDevice uint64 `json:"root_device"`
	RootInode  uint64 `json:"root_inode"`
	PID        int    `json:"pid"`
	StartedAt  string `json:"started_at"`
}

// ownerRecord is the decoded, validated record.
type ownerRecord struct {
	Schema     uint64
	Nonce      string
	Generation uint64
	RootDevice uint64
	RootInode  uint64
	PID        int
	StartedAt  string
}

func readOwnerRecord(path string) (ownerRecord, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ownerRecord{}, nil, ErrInvalidRecord
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ownerRecord{}, nil, ErrInvalidRecord
	}
	if nlink(info) > 1 {
		return ownerRecord{}, nil, ErrInvalidRecord
	}
	if info.Size() > maxRecordBytes {
		return ownerRecord{}, nil, ErrInvalidRecord
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ownerRecord{}, nil, ErrInvalidRecord
	}
	record, err := decodeOwnerRecord(data)
	if err != nil {
		return ownerRecord{}, nil, ErrInvalidRecord
	}
	return record, data, nil
}

func decodeOwnerRecord(data []byte) (ownerRecord, error) {
	if len(data) > maxRecordBytes || !utf8.Valid(data) {
		return ownerRecord{}, ErrInvalidRecord
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ownerRecord{}, ErrInvalidRecord
	}
	if err := strictJSONTokens(data); err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	allowed := map[string]bool{
		"schema": true, "nonce": true, "generation": true, "root_device": true,
		"root_inode": true, "pid": true, "started_at": true,
	}
	for key := range raw {
		if !allowed[key] {
			return ownerRecord{}, ErrInvalidRecord
		}
	}
	for _, key := range []string{"schema", "nonce", "generation", "root_device", "root_inode", "pid", "started_at"} {
		if _, ok := raw[key]; !ok {
			return ownerRecord{}, ErrInvalidRecord
		}
	}
	schema, err := decodeUint(raw["schema"])
	if err != nil || schema != 1 {
		return ownerRecord{}, ErrInvalidRecord
	}
	nonce, err := decodeString(raw["nonce"])
	if err != nil || !validNonce(nonce) {
		return ownerRecord{}, ErrInvalidRecord
	}
	generation, err := decodeUint(raw["generation"])
	if err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	rootDevice, err := decodeUint(raw["root_device"])
	if err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	rootInode, err := decodeUint(raw["root_inode"])
	if err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	pid, err := decodeInt(raw["pid"])
	if err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	startedAt, err := decodeString(raw["started_at"])
	if err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	if _, err := time.Parse(time.RFC3339, startedAt); err != nil {
		return ownerRecord{}, ErrInvalidRecord
	}
	return ownerRecord{
		Schema:     schema,
		Nonce:      nonce,
		Generation: generation,
		RootDevice: rootDevice,
		RootInode:  rootInode,
		PID:        pid,
		StartedAt:  startedAt,
	}, nil
}

// strictJSONTokens rejects invalid UTF-8, duplicate decoded member names
// (including escaped spellings), excessive nesting, and trailing data.
func strictJSONTokens(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > maxJSONDepth {
			return ErrInvalidRecord
		}
		token, err := decoder.Token()
		if err != nil {
			return ErrInvalidRecord
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return ErrInvalidRecord
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return ErrInvalidRecord
				}
				seen[key] = true
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return ErrInvalidRecord
			}
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return ErrInvalidRecord
			}
		default:
			return ErrInvalidRecord
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidRecord
	}
	return nil
}

func decodeUint(raw json.RawMessage) (uint64, error) {
	s := string(bytes.TrimSpace(raw))
	if s == "" {
		return 0, ErrInvalidRecord
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, ErrInvalidRecord
		}
	}
	return strconv.ParseUint(s, 10, 64)
}

func decodeInt(raw json.RawMessage) (int, error) {
	s := string(bytes.TrimSpace(raw))
	if s == "" {
		return 0, ErrInvalidRecord
	}
	value, err := strconv.Atoi(s)
	if err != nil {
		return 0, ErrInvalidRecord
	}
	return value, nil
}

func decodeString(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", ErrInvalidRecord
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", ErrInvalidRecord
	}
	return value, nil
}

func validNonce(nonce string) bool {
	if len(nonce) != nonceBytes*2 {
		return false
	}
	for i := 0; i < len(nonce); i++ {
		c := nonce[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func newNonce() (string, error) {
	var buf [nonceBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// nextGeneration reads the persisted counter. An absent generation file means
// the first generation (1); every existing value is incremented so later
// acquisitions strictly increase.
func nextGeneration(root string) (uint64, error) {
	path := filepath.Join(root, generationName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return 1, nil
	}
	if err != nil {
		return 0, ErrInvalidRecord
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 64 {
		return 0, ErrInvalidRecord
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, ErrInvalidRecord
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, ErrInvalidRecord
	}
	value, err := strconv.ParseUint(s, 10, 64)
	if err != nil || value == ^uint64(0) {
		return 0, ErrInvalidRecord
	}
	return value + 1, nil
}

// writeFileAtomic writes data through a same-directory temp file, fsyncs it,
// renames it into place and fsyncs the directory.
func writeFileAtomic(dir, name string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if err := file.Chmod(mode); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	return dirHandle.Sync()
}

func fileIDs(info os.FileInfo) (uint64, uint64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}

func nlink(info os.FileInfo) uint64 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
