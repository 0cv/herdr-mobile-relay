package managedstate

// S7A durable transaction journal primitives.
//
// A journal is staged entirely before any target is touched: every new byte
// string is written to a synced 0600 stage file, then a bounded strict
// journal.json (schema 1) is written with the same temp+fsync+rename+dir-fsync
// protocol used for owner.json. Apply renames each stage file over its target
// and durably rewrites journal.json after every entry, so a crash mid-apply is
// recoverable from the last durable journal state. Rollback walks entries in
// reverse, restoring prior bytes/absence atomically and refusing with
// ForeignState (retaining the journal and stage files as evidence) when a
// target matches neither the prior nor the expected new bytes. RecoverJournal
// is strictly read-only. None of these primitives is wired into a shell caller
// yet; S7B owns that wiring.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	journalName           = "journal.json"
	journalSchema         = 1
	maxJournalEntries     = 32
	maxJournalLabelBytes  = 128
	maxPriorBytesPerEntry = 8 * 1024
	maxPriorBytesTotal    = 12 * 1024
	maxNewBytesPerEntry   = 1 * 1024 * 1024
)

const (
	journalStateStaged    = "staged"
	journalStateApplied   = "applied"
	journalStateCommitted = "committed"
)

// JournalEntry is one staged file mutation. Path is a canonical root-relative
// file name (a single component: no empty, ".", "..", "/", "\\" or NUL).
type JournalEntry struct {
	Path        string
	PriorExists bool
	PriorBytes  []byte
	PriorMode   os.FileMode
	NewBytes    []byte
	NewMode     os.FileMode
}

// JournalStatus is the read-only view returned by RecoverJournal.
type JournalStatus struct {
	Present bool
	TxnID   string
	RunID   string
	State   string
	Entries int
}

// Journal is a begun durable transaction journal bound to a live Txn.
type Journal struct {
	root     *Root
	txn      *Txn
	txnID    string
	runID    string
	state    string
	entries  []journalEntryInfo
	finished bool
}

type journalEntryInfo struct {
	path        string
	priorExists bool
	priorBytes  []byte
	priorMode   os.FileMode
	newBytes    []byte
	newMode     os.FileMode
	newSHA      string
	staged      string
	applied     bool
}

// journalJSON is the exact schema-1 write shape. PriorBytes is a pointer so an
// existing empty prior file still emits "prior_bytes":"" while an absent prior
// file omits the member entirely.
type journalJSON struct {
	Schema  int                `json:"schema"`
	TxnID   string             `json:"txn_id"`
	RunID   string             `json:"run_id"`
	State   string             `json:"state"`
	Entries []journalEntryJSON `json:"entries"`
}

type journalEntryJSON struct {
	Path        string  `json:"path"`
	PriorExists bool    `json:"prior_exists"`
	PriorMode   uint32  `json:"prior_mode"`
	PriorBytes  *string `json:"prior_bytes,omitempty"`
	NewMode     uint32  `json:"new_mode"`
	NewSHA256   string  `json:"new_sha256"`
	Staged      string  `json:"staged"`
	Applied     bool    `json:"applied"`
}

// BeginJournal stages every entry's new bytes and publishes a schema-1
// journal.json before any target is touched. It requires a live, unreleased
// Txn, either owner-bound or T-only; owner-bound transactions additionally
// re-prove the owner record.
func (t *Txn) BeginJournal(txnID, runID string, entries []JournalEntry) (*Journal, error) {
	root, err := t.liveRoot()
	if err != nil {
		return nil, err
	}
	if err := validateJournalLabel(txnID); err != nil {
		return nil, err
	}
	if err := validateJournalLabel(runID); err != nil {
		return nil, err
	}
	if len(entries) == 0 || len(entries) > maxJournalEntries {
		return nil, ErrInvalidRecord
	}
	infos := make([]journalEntryInfo, len(entries))
	seen := make(map[string]bool, len(entries))
	totalPrior := 0
	for i := range entries {
		entry := entries[i]
		if err := validateJournalPath(entry.Path); err != nil {
			return nil, err
		}
		if seen[entry.Path] {
			return nil, ErrInvalidRecord
		}
		seen[entry.Path] = true
		if !regularJournalMode(entry.PriorMode) || !regularJournalMode(entry.NewMode) {
			return nil, ErrInvalidRecord
		}
		if len(entry.PriorBytes) > maxPriorBytesPerEntry {
			return nil, ErrInvalidRecord
		}
		if !entry.PriorExists && len(entry.PriorBytes) != 0 {
			return nil, ErrInvalidRecord
		}
		totalPrior += len(entry.PriorBytes)
		if totalPrior > maxPriorBytesTotal {
			return nil, ErrInvalidRecord
		}
		if len(entry.NewBytes) > maxNewBytesPerEntry {
			return nil, ErrInvalidRecord
		}
		infos[i] = journalEntryInfo{
			path:        entry.Path,
			priorExists: entry.PriorExists,
			priorBytes:  append([]byte(nil), entry.PriorBytes...),
			priorMode:   entry.PriorMode.Perm(),
			newBytes:    entry.NewBytes,
			newMode:     entry.NewMode.Perm(),
			newSHA:      sha256Hex(entry.NewBytes),
			staged:      journalStageName(txnID, i),
		}
	}

	journalPath := filepath.Join(root.path, journalName)
	if _, err := os.Lstat(journalPath); err == nil {
		// A journal already on disk is evidence for a later recovery; never
		// overwrite it.
		return nil, ErrForeignState
	} else if !os.IsNotExist(err) {
		return nil, ErrIOFailure
	}

	j := &Journal{
		root:    root,
		txn:     t,
		txnID:   txnID,
		runID:   runID,
		state:   journalStateStaged,
		entries: infos,
	}
	// The persisted record must itself stay within the decode bound for the
	// whole lifecycle: base64 expansion means a raw-prior-legal journal can
	// otherwise be written larger than RecoverJournal will accept, and the
	// committed rewrite grows the state field ("staged" -> "committed") while
	// shrinking each applied flag (false -> true), so the initial staged shape
	// is not always the largest. Bound the maximum over the shapes Apply can
	// write and reject before creating any stage file so no partial evidence is
	// left.
	maxEncoded, err := maxJournalEncodedBytes(txnID, runID, infos)
	if err != nil {
		return nil, ErrIOFailure
	}
	if maxEncoded > maxRecordBytes {
		return nil, ErrInvalidRecord
	}

	for i := range infos {
		info := &infos[i]
		stagePath := filepath.Join(root.path, info.staged)
		file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				return nil, ErrForeignState
			}
			return nil, ErrIOFailure
		}
		if _, err := file.Write(info.newBytes); err != nil {
			file.Close()
			return nil, ErrIOFailure
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return nil, ErrIOFailure
		}
		if err := file.Close(); err != nil {
			return nil, ErrIOFailure
		}
		info.newBytes = nil
	}

	if err := j.persist(); err != nil {
		return nil, err
	}
	return j, nil
}

// Apply renames each stage file over its target in order, syncing the file and
// directory, and durably rewrites journal.json after every entry so a crash
// mid-apply is recoverable. Only after a durable "committed" rewrite are the
// journal and stage files removed. On any failure the last durable journal
// stays on disk and the journal is never deleted.
func (j *Journal) Apply() error {
	if err := j.authority(); err != nil {
		return err
	}
	for i := range j.entries {
		entry := &j.entries[i]
		if entry.applied {
			continue
		}
		stagePath := filepath.Join(j.root.path, entry.staged)
		data, err := os.ReadFile(stagePath)
		if err != nil {
			return ErrIOFailure
		}
		if sha256Hex(data) != entry.newSHA {
			return ErrForeignState
		}
		target := filepath.Join(j.root.path, entry.path)
		if err := os.Rename(stagePath, target); err != nil {
			return ErrIOFailure
		}
		if err := os.Chmod(target, entry.newMode); err != nil {
			return ErrIOFailure
		}
		if err := syncPath(target); err != nil {
			return ErrIOFailure
		}
		if err := syncPath(j.root.path); err != nil {
			return ErrIOFailure
		}
		entry.applied = true
		if err := j.persist(); err != nil {
			return err
		}
	}
	j.state = journalStateCommitted
	if err := j.persist(); err != nil {
		return err
	}
	if err := j.removeJournalAndStages(); err != nil {
		return err
	}
	j.finished = true
	return nil
}

// Rollback walks entries in reverse and restores prior bytes/absence/mode
// atomically. A target that matches neither its expected new bytes nor its
// prior state means a foreign edit: Rollback returns ForeignState, stops, and
// retains the journal and stage files as evidence.
func (j *Journal) Rollback() error {
	if err := j.authority(); err != nil {
		return err
	}
	for i := len(j.entries) - 1; i >= 0; i-- {
		entry := &j.entries[i]
		kind, err := classifyJournalTarget(j.root.path, entry)
		if err != nil {
			return err
		}
		switch kind {
		case journalTargetForeign:
			return ErrForeignState
		case journalTargetPrior, journalTargetNew:
			if err := restoreJournalPrior(j.root.path, entry); err != nil {
				return err
			}
		}
		entry.applied = false
	}
	if err := j.removeJournalAndStages(); err != nil {
		return err
	}
	j.finished = true
	return nil
}

// RecoverJournal reports the durable journal state read-only. It never applies,
// rolls back, rewrites, truncates or repairs anything.
func RecoverJournal(root *Root) (JournalStatus, error) {
	if root == nil || root.closed {
		return JournalStatus{}, ErrIOFailure
	}
	path := filepath.Join(root.path, journalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return JournalStatus{Present: false}, nil
	}
	if err != nil {
		return JournalStatus{}, ErrIOFailure
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || nlink(info) > 1 {
		return JournalStatus{}, ErrInvalidRecord
	}
	if info.Size() > maxRecordBytes {
		return JournalStatus{}, ErrInvalidRecord
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return JournalStatus{}, ErrInvalidRecord
	}
	journal, err := decodeJournal(data)
	if err != nil {
		return JournalStatus{}, err
	}
	return JournalStatus{
		Present: true,
		TxnID:   journal.txnID,
		RunID:   journal.runID,
		State:   journal.state,
		Entries: len(journal.entries),
	}, nil
}

type journalTargetKind int

const (
	journalTargetForeign journalTargetKind = iota
	journalTargetPrior
	journalTargetNew
)

func classifyJournalTarget(dir string, entry *journalEntryInfo) (journalTargetKind, error) {
	target := filepath.Join(dir, entry.path)
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		if !entry.priorExists {
			return journalTargetPrior, nil
		}
		return journalTargetForeign, nil
	}
	if err != nil {
		return journalTargetForeign, ErrIOFailure
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return journalTargetForeign, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return journalTargetForeign, ErrIOFailure
	}
	if sha256Hex(data) == entry.newSHA {
		return journalTargetNew, nil
	}
	if entry.priorExists && bytes.Equal(data, entry.priorBytes) {
		return journalTargetPrior, nil
	}
	return journalTargetForeign, nil
}

func restoreJournalPrior(dir string, entry *journalEntryInfo) error {
	target := filepath.Join(dir, entry.path)
	if !entry.priorExists {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return ErrIOFailure
		}
		if err := syncPath(dir); err != nil {
			return ErrIOFailure
		}
		return nil
	}
	if err := writeFileAtomic(dir, entry.path, entry.priorBytes, entry.priorMode); err != nil {
		return ErrIOFailure
	}
	return nil
}

func (j *Journal) authority() error {
	if j == nil || j.root == nil || j.txn == nil || j.finished {
		return ErrUnknownAuthority
	}
	if j.txn.released {
		return ErrUnknownAuthority
	}
	if j.txn.owner != nil {
		if err := j.txn.owner.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// liveRoot returns the canonical root for a live, unreleased Txn after
// re-proving the Txn lock identity. Owner-bound transactions additionally
// re-prove the owner record; a T-only transaction has no owner to validate.
func (t *Txn) liveRoot() (*Root, error) {
	if t == nil || t.released || t.root == nil {
		return nil, ErrUnknownAuthority
	}
	if t.owner != nil {
		if err := t.owner.Validate(); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(t.path)
	if err != nil {
		return nil, ErrUnknownAuthority
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ErrForeignState
	}
	dev, ino := fileIDs(info)
	if dev != t.dev || ino != t.ino {
		return nil, ErrForeignState
	}
	return t.root, nil
}

func (j *Journal) persist() error {
	data, err := j.encode()
	if err != nil {
		return ErrIOFailure
	}
	// Refuse to write a record that the strict read path would reject: a
	// journal larger than the decode bound is unreadable evidence, not a
	// journal. BeginJournal already rejected any lifecycle shape over the
	// bound; this is defense in depth.
	if len(data) > maxRecordBytes {
		return ErrInvalidRecord
	}
	if err := writeFileAtomic(j.root.path, journalName, data, 0o600); err != nil {
		return ErrIOFailure
	}
	return nil
}

func (j *Journal) encode() ([]byte, error) {
	doc := journalJSON{
		Schema: journalSchema,
		TxnID:  j.txnID,
		RunID:  j.runID,
		State:  j.state,
	}
	doc.Entries = make([]journalEntryJSON, 0, len(j.entries))
	for i := range j.entries {
		entry := &j.entries[i]
		encoded := journalEntryJSON{
			Path:        entry.path,
			PriorExists: entry.priorExists,
			PriorMode:   uint32(entry.priorMode.Perm()),
			NewMode:     uint32(entry.newMode.Perm()),
			NewSHA256:   entry.newSHA,
			Staged:      entry.staged,
			Applied:     entry.applied,
		}
		if entry.priorExists {
			value := base64.StdEncoding.EncodeToString(entry.priorBytes)
			encoded.PriorBytes = &value
		}
		doc.Entries = append(doc.Entries, encoded)
	}
	return json.Marshal(doc)
}

// maxJournalEncodedBytes returns the largest encoded size over the lifecycle
// shapes Apply can write: the initial staged/all-false shape and the final
// committed/all-true shape. Apply's intermediate writes use state "staged"
// with a non-empty applied prefix, which is never larger than the initial
// all-false shape, so these two probes bound the writer.
func maxJournalEncodedBytes(txnID, runID string, entries []journalEntryInfo) (int, error) {
	probe := func(state string, applied bool) (int, error) {
		j := &Journal{txnID: txnID, runID: runID, state: state, entries: make([]journalEntryInfo, len(entries))}
		for i := range entries {
			j.entries[i] = entries[i]
			j.entries[i].applied = applied
		}
		data, err := j.encode()
		if err != nil {
			return 0, err
		}
		return len(data), nil
	}
	staged, err := probe(journalStateStaged, false)
	if err != nil {
		return 0, err
	}
	committed, err := probe(journalStateCommitted, true)
	if err != nil {
		return 0, err
	}
	if committed > staged {
		return committed, nil
	}
	return staged, nil
}

func (j *Journal) removeJournalAndStages() error {
	journalPath := filepath.Join(j.root.path, journalName)
	if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		return ErrIOFailure
	}
	for i := range j.entries {
		stagePath := filepath.Join(j.root.path, j.entries[i].staged)
		if err := os.Remove(stagePath); err != nil && !os.IsNotExist(err) {
			return ErrIOFailure
		}
	}
	if err := syncPath(j.root.path); err != nil {
		return ErrIOFailure
	}
	return nil
}

func decodeJournal(data []byte) (*Journal, error) {
	if len(data) > maxRecordBytes || !utf8.Valid(data) {
		return nil, ErrInvalidRecord
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ErrInvalidRecord
	}
	if err := strictJSONTokens(data); err != nil {
		return nil, ErrInvalidRecord
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return nil, ErrInvalidRecord
	}
	for key := range raw {
		switch key {
		case "schema", "txn_id", "run_id", "state", "entries":
		default:
			return nil, ErrInvalidRecord
		}
	}
	for _, key := range []string{"schema", "txn_id", "run_id", "state", "entries"} {
		if _, ok := raw[key]; !ok {
			return nil, ErrInvalidRecord
		}
	}
	schema, err := decodeUint(raw["schema"])
	if err != nil || schema != journalSchema {
		return nil, ErrInvalidRecord
	}
	txnID, err := decodeString(raw["txn_id"])
	if err != nil || validateJournalLabel(txnID) != nil {
		return nil, ErrInvalidRecord
	}
	runID, err := decodeString(raw["run_id"])
	if err != nil || validateJournalLabel(runID) != nil {
		return nil, ErrInvalidRecord
	}
	state, err := decodeString(raw["state"])
	if err != nil || !validJournalState(state) {
		return nil, ErrInvalidRecord
	}
	rawEntries, err := decodeRawArray(raw["entries"])
	if err != nil || len(rawEntries) == 0 || len(rawEntries) > maxJournalEntries {
		return nil, ErrInvalidRecord
	}
	entries := make([]journalEntryInfo, len(rawEntries))
	seen := make(map[string]bool, len(rawEntries))
	totalPrior := 0
	for i := range rawEntries {
		entry, err := decodeJournalEntry(rawEntries[i], txnID, i)
		if err != nil {
			return nil, err
		}
		if seen[entry.path] {
			return nil, ErrInvalidRecord
		}
		seen[entry.path] = true
		totalPrior += len(entry.priorBytes)
		if totalPrior > maxPriorBytesTotal {
			return nil, ErrInvalidRecord
		}
		entries[i] = entry
	}
	return &Journal{txnID: txnID, runID: runID, state: state, entries: entries}, nil
}

func decodeJournalEntry(raw json.RawMessage, txnID string, index int) (journalEntryInfo, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	allowed := map[string]bool{
		"path": true, "prior_exists": true, "prior_mode": true, "prior_bytes": true,
		"new_mode": true, "new_sha256": true, "staged": true, "applied": true,
	}
	for key := range fields {
		if !allowed[key] {
			return journalEntryInfo{}, ErrInvalidRecord
		}
	}
	for _, key := range []string{"path", "prior_exists", "prior_mode", "new_mode", "new_sha256", "staged", "applied"} {
		if _, ok := fields[key]; !ok {
			return journalEntryInfo{}, ErrInvalidRecord
		}
	}
	path, err := decodeString(fields["path"])
	if err != nil || validateJournalPath(path) != nil {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	priorExists, err := decodeBool(fields["prior_exists"])
	if err != nil {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	priorMode, err := decodeUint(fields["prior_mode"])
	if err != nil || priorMode > 0o777 || !regularJournalMode(os.FileMode(priorMode)) {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	newMode, err := decodeUint(fields["new_mode"])
	if err != nil || newMode > 0o777 || !regularJournalMode(os.FileMode(newMode)) {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	newSHA, err := decodeString(fields["new_sha256"])
	if err != nil || !validJournalSHA(newSHA) {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	staged, err := decodeString(fields["staged"])
	if err != nil || staged != journalStageName(txnID, index) {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	applied, err := decodeBool(fields["applied"])
	if err != nil {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	var priorBytes []byte
	if rawPrior, present := fields["prior_bytes"]; present {
		encoded, err := decodeString(rawPrior)
		if err != nil {
			return journalEntryInfo{}, ErrInvalidRecord
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(decoded) > maxPriorBytesPerEntry {
			return journalEntryInfo{}, ErrInvalidRecord
		}
		priorBytes = decoded
	}
	if priorExists {
		if _, present := fields["prior_bytes"]; !present {
			return journalEntryInfo{}, ErrInvalidRecord
		}
	} else if len(priorBytes) != 0 {
		return journalEntryInfo{}, ErrInvalidRecord
	}
	return journalEntryInfo{
		path:        path,
		priorExists: priorExists,
		priorBytes:  priorBytes,
		priorMode:   os.FileMode(priorMode),
		newMode:     os.FileMode(newMode),
		newSHA:      newSHA,
		staged:      staged,
		applied:     applied,
	}, nil
}

func decodeRawArray(raw json.RawMessage) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, ErrInvalidRecord
	}
	var array []json.RawMessage
	if err := json.Unmarshal(raw, &array); err != nil {
		return nil, ErrInvalidRecord
	}
	return array, nil
}

func decodeBool(raw json.RawMessage) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, ErrInvalidRecord
	}
}

func validateJournalLabel(label string) error {
	if label == "" || len(label) > maxJournalLabelBytes {
		return ErrInvalidRecord
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if c < 0x20 || c > 0x7e || c == '/' || c == '\\' {
			return ErrInvalidRecord
		}
	}
	return nil
}

func validateJournalPath(path string) error {
	if path == "" || path == "." || path == ".." {
		return ErrInvalidRecord
	}
	if strings.ContainsAny(path, "\x00/\\") {
		return ErrInvalidRecord
	}
	if filepath.Base(path) != path || filepath.Clean(path) != path {
		return ErrInvalidRecord
	}
	if reservedJournalPath(path) {
		return ErrInvalidRecord
	}
	return nil
}

// reservedJournalPath rejects the journal's own control names and the other
// managed root control objects. Without this an entry could rename its stage
// file over journal.json (destroying the durable record before the committed
// marker) or over the owner/transaction locks and generation counter.
func reservedJournalPath(path string) bool {
	switch path {
	case journalName, ownerLockName, txnLockName, generationName:
		return true
	}
	return strings.HasPrefix(path, "journal.stage.")
}

func regularJournalMode(mode os.FileMode) bool {
	if mode&^os.FileMode(0o777) != 0 {
		return false
	}
	perm := mode.Perm()
	return perm == 0o600 || perm == 0o644
}

func validJournalState(state string) bool {
	switch state {
	case journalStateStaged, journalStateApplied, journalStateCommitted:
		return true
	default:
		return false
	}
}

func validJournalSHA(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func journalStageName(txnID string, index int) string {
	return fmt.Sprintf("journal.stage.%s.%d", txnID, index)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func syncPath(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
