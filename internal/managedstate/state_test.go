package managedstate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/managedstate"
)

func TestOpenExistingRootRequiresPrivateExistingDirectory(t *testing.T) {
	base := t.TempDir()

	if _, err := managedstate.OpenExistingRoot(filepath.Join(base, "missing")); !errors.Is(err, managedstate.ErrIOFailure) {
		t.Fatalf("missing path: got %v, want %v", err, managedstate.ErrIOFailure)
	}

	regular := filepath.Join(base, "regular")
	writeFile(t, regular, []byte("x"), 0o600)
	if _, err := managedstate.OpenExistingRoot(regular); !errors.Is(err, managedstate.ErrIOFailure) {
		t.Fatalf("regular file: got %v, want %v", err, managedstate.ErrIOFailure)
	}

	public := filepath.Join(base, "public")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatalf("mkdir public: %v", err)
	}
	if _, err := managedstate.OpenExistingRoot(public); !errors.Is(err, managedstate.ErrIOFailure) {
		t.Fatalf("wrong mode: got %v, want %v", err, managedstate.ErrIOFailure)
	}

	private := filepath.Join(base, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatalf("mkdir private: %v", err)
	}
	root := openRoot(t, private)
	noErr(t, root.Close(), "first close")
	noErr(t, root.Close(), "second close")
}

func TestAliasedRootsContendForOwner(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatalf("symlink alias: %v", err)
	}

	direct := openRoot(t, dir)
	owner := acquireOwner(t, direct)
	publishOwner(t, owner)

	aliased := openRoot(t, alias)
	if _, err := aliased.TryAcquireOwner(); !errors.Is(err, managedstate.ErrBusy) {
		t.Fatalf("aliased owner: got %v, want %v", err, managedstate.ErrBusy)
	}
}

func TestOwnerExclusionReturnsBusy(t *testing.T) {
	dir := mkRoot(t)
	first := openRoot(t, dir)
	owner := acquireOwner(t, first)
	publishOwner(t, owner)

	ownerPath := filepath.Join(dir, "owner.lock", "owner.json")
	before := readFile(t, ownerPath)

	second := openRoot(t, dir)
	if _, err := second.TryAcquireOwner(); !errors.Is(err, managedstate.ErrBusy) {
		t.Fatalf("second owner: got %v, want %v", err, managedstate.ErrBusy)
	}

	noErr(t, owner.Validate(), "first owner validate")
	after := readFile(t, ownerPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("first owner record changed: %q -> %q", before, after)
	}
}

func TestOwnerRecordRoundTripAndBounds(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	ownerPath := filepath.Join(dir, "owner.lock", "owner.json")
	raw := readFile(t, ownerPath)
	nonce, generation := recordNonceGen(t, raw)

	var fields map[string]json.RawMessage
	noErr(t, json.Unmarshal(raw, &fields), "decode record")
	var schema int
	noErr(t, json.Unmarshal(fields["schema"], &schema), "decode schema")
	if schema != 1 {
		t.Fatalf("schema = %d, want 1", schema)
	}
	if len(nonce) != 64 {
		t.Fatalf("nonce length = %d, want 64", len(nonce))
	}
	if generation == 0 {
		t.Fatalf("generation = 0, want >= 1")
	}
	noErr(t, owner.Validate(), "validate published record")

	cases := []struct {
		name string
		data string
	}{
		{"oversized", strings.Repeat("a", 16*1024+1)},
		{"duplicate-key", fmt.Sprintf(`{"schema":1,"nonce":%q,"nonce":%q,"generation":%d,"root_device":1,"root_inode":2,"pid":3,"started_at":"2024-01-02T03:04:05Z"}`, nonce, nonce, generation)},
		{"unknown-key", fmt.Sprintf(`{"schema":1,"nonce":%q,"generation":%d,"root_device":1,"root_inode":2,"pid":3,"started_at":"2024-01-02T03:04:05Z","extra":1}`, nonce, generation)},
		{"wrong-type", fmt.Sprintf(`{"schema":1,"nonce":%q,"generation":"5","root_device":1,"root_inode":2,"pid":3,"started_at":"2024-01-02T03:04:05Z"}`, nonce)},
		{"truncated", `{"schema":1,`},
	}
	for _, tc := range cases {
		writeFile(t, ownerPath, []byte(tc.data), 0o600)
		if err := owner.Validate(); !errors.Is(err, managedstate.ErrInvalidRecord) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, managedstate.ErrInvalidRecord)
		}
	}
}

func TestOwnerValidateDetectsChangedRoot(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	moved := filepath.Join(base, "moved")
	noErr(t, os.Rename(dir, moved), "rename root")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("recreate root: %v", err)
	}
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrChangedRoot) {
		t.Fatalf("changed root: got %v, want %v", err, managedstate.ErrChangedRoot)
	}
	if _, err := os.Lstat(filepath.Join(moved, "owner.lock", "owner.json")); err != nil {
		t.Fatalf("moved record not preserved: %v", err)
	}
}

func TestOwnerValidateDetectsRecordTamper(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	ownerPath := filepath.Join(dir, "owner.lock", "owner.json")
	nonce, generation := recordNonceGen(t, readFile(t, ownerPath))

	writeFile(t, ownerPath, []byte(recordJSON(strings.Repeat("b", 64), generation)), 0o600)
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("different nonce: got %v, want %v", err, managedstate.ErrForeignState)
	}

	writeFile(t, ownerPath, []byte(recordJSON(nonce, generation+7)), 0o600)
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("different generation: got %v, want %v", err, managedstate.ErrForeignState)
	}

	target := filepath.Join(dir, "target.json")
	writeFile(t, target, []byte("{}"), 0o600)
	noErr(t, os.Remove(ownerPath), "remove record")
	noErr(t, os.Symlink(target, ownerPath), "symlink record")
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrInvalidRecord) {
		t.Fatalf("symlinked record: got %v, want %v", err, managedstate.ErrInvalidRecord)
	}

	if fi, err := os.Lstat(ownerPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("record not preserved: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "owner.lock")); err != nil || !fi.IsDir() {
		t.Fatalf("lock not preserved: %v", err)
	}
	if got := readFile(t, target); string(got) != "{}" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestTransactionContentionTimesOut(t *testing.T) {
	dir := mkRoot(t)
	rootA := openRoot(t, dir)
	ownerA := acquireOwner(t, rootA)
	publishOwner(t, ownerA)
	txnA := acquireTxn(t, ownerA, 0)

	// O is exclusive, so the only cooperating way to let a second owner exist
	// to contend on T is to retire A's owner lock while A still holds T.
	noErr(t, ownerA.BeginClosing(), "begin closing A")
	noErr(t, ownerA.Retire(), "retire A")

	rootB := openRoot(t, dir)
	ownerB := acquireOwner(t, rootB)
	publishOwner(t, ownerB)
	if _, err := ownerB.AcquireTransaction(context.Background(), 40*time.Millisecond); !errors.Is(err, managedstate.ErrTimeout) {
		t.Fatalf("contended T: got %v, want %v", err, managedstate.ErrTimeout)
	}

	noErr(t, txnA.Release(), "release A T")
	txnB := acquireTxn(t, ownerB, 0)
	noErr(t, txnB.Release(), "release B T")
}

func TestTransactionReleaseAndNonRecursive(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	first := acquireTxn(t, owner, 0)
	if _, err := owner.AcquireTransaction(context.Background(), 0); !errors.Is(err, managedstate.ErrBusy) {
		t.Fatalf("recursive T: got %v, want %v", err, managedstate.ErrBusy)
	}
	noErr(t, first.Release(), "release first")
	second := acquireTxn(t, owner, 0)
	noErr(t, second.Release(), "release second")
	if err := second.Release(); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("double release: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}
}

func TestClosingOwnerRefusesTransactions(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	noErr(t, owner.BeginClosing(), "begin closing")
	if _, err := owner.AcquireTransaction(context.Background(), 0); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("txn after closing: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}
	if err := owner.PublishRecord(); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("publish after closing: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}
	noErr(t, owner.Validate(), "validate after closing")
}

func TestPartialLockRetainedEvidenceNoReclaim(t *testing.T) {
	base := t.TempDir()

	empty := filepath.Join(base, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	if err := os.Mkdir(filepath.Join(empty, "owner.lock"), 0o700); err != nil {
		t.Fatalf("mkdir empty lock: %v", err)
	}
	emptyRoot := openRoot(t, empty)
	if _, err := emptyRoot.TryAcquireOwner(); !errors.Is(err, managedstate.ErrRetainedEvidence) {
		t.Fatalf("empty lock: got %v, want %v", err, managedstate.ErrRetainedEvidence)
	}
	entries, err := os.ReadDir(filepath.Join(empty, "owner.lock"))
	if err != nil {
		t.Fatalf("read empty lock: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty lock changed: %v", entries)
	}

	partial := filepath.Join(base, "partial")
	if err := os.Mkdir(partial, 0o700); err != nil {
		t.Fatalf("mkdir partial: %v", err)
	}
	if err := os.Mkdir(filepath.Join(partial, "owner.lock"), 0o700); err != nil {
		t.Fatalf("mkdir partial lock: %v", err)
	}
	partialPath := filepath.Join(partial, "owner.lock", "owner.json")
	partialBytes := []byte(`{"schema":1`)
	writeFile(t, partialPath, partialBytes, 0o600)
	partialRoot := openRoot(t, partial)
	if _, err := partialRoot.TryAcquireOwner(); !errors.Is(err, managedstate.ErrRetainedEvidence) {
		t.Fatalf("partial lock: got %v, want %v", err, managedstate.ErrRetainedEvidence)
	}
	if got := readFile(t, partialPath); !bytes.Equal(got, partialBytes) {
		t.Fatalf("partial record changed: %q", got)
	}
}

func TestForeignLockStatesRefusedAndPreserved(t *testing.T) {
	base := t.TempDir()

	symRoot := filepath.Join(base, "sym")
	if err := os.Mkdir(symRoot, 0o700); err != nil {
		t.Fatalf("mkdir sym: %v", err)
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	noErr(t, os.Symlink(target, filepath.Join(symRoot, "owner.lock")), "symlink lock")
	symAlias := openRoot(t, symRoot)
	if _, err := symAlias.TryAcquireOwner(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("symlink lock: got %v, want %v", err, managedstate.ErrForeignState)
	}
	if fi, err := os.Lstat(filepath.Join(symRoot, "owner.lock")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink lock not preserved: %v", err)
	}

	fileRoot := filepath.Join(base, "file")
	if err := os.Mkdir(fileRoot, 0o700); err != nil {
		t.Fatalf("mkdir file root: %v", err)
	}
	lockFile := filepath.Join(fileRoot, "owner.lock")
	writeFile(t, lockFile, []byte("x"), 0o600)
	fileAlias := openRoot(t, fileRoot)
	if _, err := fileAlias.TryAcquireOwner(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("file lock: got %v, want %v", err, managedstate.ErrForeignState)
	}
	if got := readFile(t, lockFile); string(got) != "x" {
		t.Fatalf("file lock changed: %q", got)
	}

	foreignRoot := filepath.Join(base, "foreign")
	if err := os.Mkdir(foreignRoot, 0o700); err != nil {
		t.Fatalf("mkdir foreign root: %v", err)
	}
	foreign := openRoot(t, foreignRoot)
	owner := acquireOwner(t, foreign)
	foreignDir := filepath.Join(foreignRoot, "owner.lock", "owner.json")
	if err := os.Mkdir(foreignDir, 0o700); err != nil {
		t.Fatalf("mkdir foreign record: %v", err)
	}
	if err := owner.PublishRecord(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("foreign record publish: got %v, want %v", err, managedstate.ErrForeignState)
	}
	if fi, err := os.Lstat(foreignDir); err != nil || !fi.IsDir() {
		t.Fatalf("foreign record not preserved: %v", err)
	}
}

func TestHardLinkedRecordRefused(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	ownerPath := filepath.Join(dir, "owner.lock", "owner.json")
	linked := filepath.Join(dir, "owner.lock", "owner-link.json")
	noErr(t, os.Link(ownerPath, linked), "hard link record")
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrInvalidRecord) {
		t.Fatalf("hard linked record: got %v, want %v", err, managedstate.ErrInvalidRecord)
	}
	if _, err := os.Lstat(ownerPath); err != nil {
		t.Fatalf("record removed: %v", err)
	}
	if _, err := os.Lstat(linked); err != nil {
		t.Fatalf("link removed: %v", err)
	}
}

func TestRetireRefusesMismatchAndPreservesState(t *testing.T) {
	openCase := mkRoot(t)
	openRootHandle := openRoot(t, openCase)
	openOwner := acquireOwner(t, openRootHandle)
	publishOwner(t, openOwner)
	if err := openOwner.Retire(); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("retire without closing: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}

	tamperCase := mkRoot(t)
	tamperRoot := openRoot(t, tamperCase)
	tamperOwner := acquireOwner(t, tamperRoot)
	publishOwner(t, tamperOwner)
	ownerPath := filepath.Join(tamperCase, "owner.lock", "owner.json")
	nonce, generation := recordNonceGen(t, readFile(t, ownerPath))
	noErr(t, tamperOwner.BeginClosing(), "begin closing")
	writeFile(t, ownerPath, []byte(recordJSON(nonce, generation+1)), 0o600)
	if err := tamperOwner.Retire(); !errors.Is(err, managedstate.ErrForeignState) {
		t.Fatalf("retire tampered: got %v, want %v", err, managedstate.ErrForeignState)
	}
	if _, err := os.Lstat(ownerPath); err != nil {
		t.Fatalf("record removed: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(tamperCase, "owner.lock")); err != nil || !fi.IsDir() {
		t.Fatalf("lock removed: %v", err)
	}
}

func TestRetireRemovesOwnStateAfterRelease(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	txn := acquireTxn(t, owner, 0)
	noErr(t, txn.Release(), "release txn")
	noErr(t, owner.BeginClosing(), "begin closing")
	noErr(t, owner.Retire(), "retire")

	if _, err := os.Lstat(filepath.Join(dir, "owner.lock")); !os.IsNotExist(err) {
		t.Fatalf("owner.lock still present: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "generation")); err != nil {
		t.Fatalf("generation not retained: %v", err)
	}
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrUnknownAuthority) {
		t.Fatalf("validate after retire: got %v, want %v", err, managedstate.ErrUnknownAuthority)
	}
	noErr(t, owner.Close(), "close after retire")
}

func TestGenerationIncreasesAcrossAcquisitions(t *testing.T) {
	dir := mkRoot(t)
	first := lifecycleGeneration(t, dir)
	second := lifecycleGeneration(t, dir)
	if second <= first {
		t.Fatalf("generation did not increase: %d then %d", first, second)
	}

	generationPath := filepath.Join(dir, "generation")
	info, err := os.Stat(generationPath)
	if err != nil {
		t.Fatalf("stat generation: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("generation mode = %o, want 0600", info.Mode().Perm())
	}
	if info.Size() > 64 {
		t.Fatalf("generation size = %d, want <= 64", info.Size())
	}
	raw := readFile(t, generationPath)
	if _, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err != nil {
		t.Fatalf("generation not decimal: %q", raw)
	}
}

func TestPermissionAndSymlinkGuards(t *testing.T) {
	dir := mkRoot(t)
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)

	assertMode(t, filepath.Join(dir, "owner.lock"), 0o700)
	assertMode(t, filepath.Join(dir, "owner.lock", "owner.json"), 0o600)
	txn := acquireTxn(t, owner, 0)
	assertMode(t, filepath.Join(dir, "txn.lock"), 0o700)
	noErr(t, txn.Release(), "release txn")

	ownerPath := filepath.Join(dir, "owner.lock", "owner.json")
	target := filepath.Join(dir, "target.json")
	writeFile(t, target, []byte("keep"), 0o600)
	noErr(t, os.Remove(ownerPath), "remove record")
	noErr(t, os.Symlink(target, ownerPath), "symlink record")
	if err := owner.Validate(); !errors.Is(err, managedstate.ErrInvalidRecord) {
		t.Fatalf("symlinked record: got %v, want %v", err, managedstate.ErrInvalidRecord)
	}
	if got := readFile(t, target); string(got) != "keep" {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func lifecycleGeneration(t *testing.T, dir string) uint64 {
	t.Helper()
	root := openRoot(t, dir)
	owner := acquireOwner(t, root)
	publishOwner(t, owner)
	_, generation := recordNonceGen(t, readFile(t, filepath.Join(dir, "owner.lock", "owner.json")))
	noErr(t, owner.BeginClosing(), "begin closing")
	noErr(t, owner.Retire(), "retire")
	noErr(t, owner.Close(), "close owner")
	noErr(t, root.Close(), "close root")
	return generation
}

func mkRoot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	return dir
}

func openRoot(t *testing.T, path string) *managedstate.Root {
	t.Helper()
	root, err := managedstate.OpenExistingRoot(path)
	if err != nil {
		t.Fatalf("OpenExistingRoot(%q): %v", path, err)
	}
	return root
}

func acquireOwner(t *testing.T, root *managedstate.Root) *managedstate.Owner {
	t.Helper()
	owner, err := root.TryAcquireOwner()
	if err != nil {
		t.Fatalf("TryAcquireOwner: %v", err)
	}
	return owner
}

func publishOwner(t *testing.T, owner *managedstate.Owner) {
	t.Helper()
	if err := owner.PublishRecord(); err != nil {
		t.Fatalf("PublishRecord: %v", err)
	}
}

func acquireTxn(t *testing.T, owner *managedstate.Owner, limit time.Duration) *managedstate.Txn {
	t.Helper()
	txn, err := owner.AcquireTransaction(context.Background(), limit)
	if err != nil {
		t.Fatalf("AcquireTransaction: %v", err)
	}
	return txn
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func noErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func recordNonceGen(t *testing.T, raw []byte) (string, uint64) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	var nonce string
	if err := json.Unmarshal(fields["nonce"], &nonce); err != nil {
		t.Fatalf("decode nonce: %v", err)
	}
	var generation uint64
	if err := json.Unmarshal(fields["generation"], &generation); err != nil {
		t.Fatalf("decode generation: %v", err)
	}
	return nonce, generation
}

func recordJSON(nonce string, generation uint64) string {
	return fmt.Sprintf(`{"schema":1,"nonce":%q,"generation":%d,"root_device":1,"root_inode":2,"pid":3,"started_at":"2024-01-02T03:04:05Z"}`, nonce, generation)
}
