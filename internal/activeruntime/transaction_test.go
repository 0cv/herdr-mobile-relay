package activeruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTopologyTransactionBlocksRecoveryUntilDurablyResolved(t *testing.T) {
	root, activePath := transactionFixture(t)
	record := TopologyTransactionRecord{
		Generation: "g1",
		RequestID:  "request-1",
		Action:     "agent_stop",
		Target:     "pane-1",
		Panes: []TopologyPane{{
			PaneID: "pane-1", NativeSessionID: "session-1", ProfileID: "personal", WorkspaceID: "workspace-1",
		}},
	}
	transaction, err := BeginTopologyTransaction(activePath, record)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := TopologyTransactionPending(activePath)
	if err != nil || !pending {
		t.Fatalf("pending transaction = (%t, %v)", pending, err)
	}
	marker := filepath.Join(root, TopologyTransactionName)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"schema_version":1`, `"generation":"g1"`, `"request_id":"request-1"`, `"action":"agent_stop"`, `"target":"pane-1"`, `"pane_id":"pane-1"`} {
		if !strings.Contains(string(data), value) {
			t.Errorf("marker %q does not contain %s", data, value)
		}
	}
	if info, err := os.Lstat(marker); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = (%v, %v)", info, err)
	}
	if _, err := BeginTopologyTransaction(activePath, record); err == nil {
		t.Fatal("a second topology transaction replaced the unresolved marker")
	}
	if err := transaction.Resolve(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Resolve(); err != nil {
		t.Fatalf("idempotent resolve = %v", err)
	}
	pending, err = TopologyTransactionPending(activePath)
	if err != nil || pending {
		t.Fatalf("resolved transaction = (%t, %v)", pending, err)
	}
}

func TestTopologyTransactionRejectsUnsafeIdentityAndFilesystemShapes(t *testing.T) {
	root, activePath := transactionFixture(t)
	valid := TopologyTransactionRecord{Generation: "g1", RequestID: "request", Action: "agent_start"}
	tests := []struct {
		name   string
		path   string
		record TopologyTransactionRecord
	}{
		{name: "relative path", path: "active-runtime.json", record: valid},
		{name: "wrong basename", path: filepath.Join(root, "other.json"), record: valid},
		{name: "wrong generation", path: activePath, record: TopologyTransactionRecord{Generation: "g2", RequestID: "request", Action: "agent_start"}},
		{name: "blank request", path: activePath, record: TopologyTransactionRecord{Generation: "g1", Action: "agent_start"}},
		{name: "unsafe action", path: activePath, record: TopologyTransactionRecord{Generation: "g1", RequestID: "request", Action: "agent\nstart"}},
		{name: "duplicate pane", path: activePath, record: TopologyTransactionRecord{Generation: "g1", RequestID: "request", Action: "agent_start", Panes: []TopologyPane{{PaneID: "pane", NativeSessionID: "one", ProfileID: "p"}, {PaneID: "pane", NativeSessionID: "two", ProfileID: "p"}}}},
		{name: "duplicate native session", path: activePath, record: TopologyTransactionRecord{Generation: "g1", RequestID: "request", Action: "agent_start", Panes: []TopologyPane{{PaneID: "one", NativeSessionID: "same", ProfileID: "p"}, {PaneID: "two", NativeSessionID: "same", ProfileID: "p"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BeginTopologyTransaction(test.path, test.record); err == nil {
				t.Fatal("unsafe transaction was accepted")
			}
		})
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginTopologyTransaction(activePath, valid); err == nil {
		t.Fatal("transaction was created in a public runtime root")
	}
}

func TestTopologyTransactionCoversEveryDurableWriteBoundary(t *testing.T) {
	_, activePath := transactionFixture(t)
	validRecord := TopologyTransactionRecord{Generation: "g1", RequestID: "request", Action: "agent_start"}
	active := Snapshot{Generation: "g1"}
	base := func() (topologyTransactionIO, *fakeTopologyRoot, *fakeTopologyFile) {
		file := &fakeTopologyFile{info: transactionRegularInfo(t)}
		root := &fakeTopologyRoot{file: file, directory: &fakeTopologyDirectory{}}
		return topologyTransactionIO{
			load:     func(string) (Snapshot, error) { return active, nil },
			marshal:  func(value any) ([]byte, error) { return []byte(`{"ok":true}`), nil },
			openRoot: func(string) (topologyRoot, error) { return root, nil },
		}, root, file
	}
	tests := []struct {
		name   string
		mutate func(*topologyTransactionIO, *fakeTopologyRoot, *fakeTopologyFile)
	}{
		{name: "load", mutate: func(ops *topologyTransactionIO, _ *fakeTopologyRoot, _ *fakeTopologyFile) {
			ops.load = func(string) (Snapshot, error) { return Snapshot{}, errors.New("load") }
		}},
		{name: "marshal", mutate: func(ops *topologyTransactionIO, _ *fakeTopologyRoot, _ *fakeTopologyFile) {
			ops.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
		}},
		{name: "open root", mutate: func(ops *topologyTransactionIO, _ *fakeTopologyRoot, _ *fakeTopologyFile) {
			ops.openRoot = func(string) (topologyRoot, error) { return nil, errors.New("open") }
		}},
		{name: "create marker", mutate: func(_ *topologyTransactionIO, root *fakeTopologyRoot, _ *fakeTopologyFile) {
			root.openFileErr = errors.New("create")
		}},
		{name: "marker stat", mutate: func(_ *topologyTransactionIO, _ *fakeTopologyRoot, file *fakeTopologyFile) {
			file.statErr = errors.New("stat")
		}},
		{name: "unsafe marker", mutate: func(_ *topologyTransactionIO, _ *fakeTopologyRoot, file *fakeTopologyFile) {
			file.info = transactionDirectoryInfo(t)
		}},
		{name: "marker write", mutate: func(_ *topologyTransactionIO, _ *fakeTopologyRoot, file *fakeTopologyFile) {
			file.writeErr = errors.New("write")
		}},
		{name: "short marker write", mutate: func(_ *topologyTransactionIO, _ *fakeTopologyRoot, file *fakeTopologyFile) { file.shortWrite = true }},
		{name: "marker sync", mutate: func(_ *topologyTransactionIO, _ *fakeTopologyRoot, file *fakeTopologyFile) {
			file.syncErr = errors.New("sync")
		}},
		{name: "marker close", mutate: func(_ *topologyTransactionIO, _ *fakeTopologyRoot, file *fakeTopologyFile) {
			file.closeErr = errors.New("close")
		}},
		{name: "directory open", mutate: func(_ *topologyTransactionIO, root *fakeTopologyRoot, _ *fakeTopologyFile) {
			root.openDirectoryErr = errors.New("directory open")
		}},
		{name: "directory sync", mutate: func(_ *topologyTransactionIO, root *fakeTopologyRoot, _ *fakeTopologyFile) {
			root.directory.(*fakeTopologyDirectory).syncErr = errors.New("directory sync")
		}},
		{name: "directory close", mutate: func(_ *topologyTransactionIO, root *fakeTopologyRoot, _ *fakeTopologyFile) {
			root.directory.(*fakeTopologyDirectory).closeErr = errors.New("directory close")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ops, root, file := base()
			test.mutate(&ops, root, file)
			if transaction, err := beginTopologyTransactionWith(ops, activePath, validRecord); err == nil || transaction != nil {
				t.Fatalf("boundary result = (%v, %v)", transaction, err)
			}
		})
	}

	huge := validRecord
	for index := 0; index < 40; index++ {
		suffix := fmt.Sprintf("-%d", index)
		huge.Panes = append(huge.Panes, TopologyPane{
			PaneID: strings.Repeat("p", 500) + suffix, NativeSessionID: strings.Repeat("s", 500) + suffix,
			ProfileID: strings.Repeat("r", 500) + suffix, WorkspaceID: strings.Repeat("w", 500) + suffix,
		})
	}
	ops, _, _ := base()
	ops.marshal = func(value any) ([]byte, error) { return make([]byte, maxTransactionBytes+1), nil }
	if _, err := beginTopologyTransactionWith(ops, activePath, huge); err == nil {
		t.Fatal("oversized transaction was accepted")
	}
}

func TestTopologyTransactionPendingCoversEveryInspectionOutcome(t *testing.T) {
	_, activePath := transactionFixture(t)
	for _, test := range []struct {
		name     string
		path     string
		openRoot func(string) (topologyRoot, error)
		want     bool
		wantErr  bool
	}{
		{name: "invalid path", path: "relative", openRoot: func(string) (topologyRoot, error) { return nil, nil }, want: true, wantErr: true},
		{name: "open", path: activePath, openRoot: func(string) (topologyRoot, error) { return nil, errors.New("open") }, want: true, wantErr: true},
		{name: "pending", path: activePath, openRoot: func(string) (topologyRoot, error) {
			return &fakeTopologyRoot{lstatInfo: transactionRegularInfo(t)}, nil
		}, want: true},
		{name: "absent", path: activePath, openRoot: func(string) (topologyRoot, error) { return &fakeTopologyRoot{lstatErr: os.ErrNotExist}, nil }},
		{name: "inspect", path: activePath, openRoot: func(string) (topologyRoot, error) { return &fakeTopologyRoot{lstatErr: errors.New("inspect")}, nil }, want: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pending, err := topologyTransactionPendingWith(test.openRoot, test.path)
			if pending != test.want || (err != nil) != test.wantErr {
				t.Fatalf("pending result = (%t, %v)", pending, err)
			}
		})
	}
}

func TestTopologyTransactionResolveCoversRemovalSyncAndCloseFailures(t *testing.T) {
	if err := (*TopologyTransaction)(nil).Resolve(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		root *fakeTopologyRoot
	}{
		{name: "remove", root: &fakeTopologyRoot{removeErr: errors.New("remove")}},
		{name: "directory open", root: &fakeTopologyRoot{openDirectoryErr: errors.New("open")}},
		{name: "directory sync", root: &fakeTopologyRoot{directory: &fakeTopologyDirectory{syncErr: errors.New("sync")}}},
		{name: "directory close", root: &fakeTopologyRoot{directory: &fakeTopologyDirectory{closeErr: errors.New("close")}}},
		{name: "root close", root: &fakeTopologyRoot{directory: &fakeTopologyDirectory{}, closeErr: errors.New("close")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			transaction := &TopologyTransaction{root: test.root}
			if err := transaction.Resolve(); err == nil {
				t.Fatal("resolve boundary failure was ignored")
			}
			if err := transaction.Resolve(); err == nil {
				t.Fatal("resolve did not preserve its first error")
			}
		})
	}
}

func TestOpenRuntimeRootCoversEveryPinnedDirectoryBoundary(t *testing.T) {
	root, activePath := transactionFixture(t)
	good, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	otherRoot := t.TempDir()
	if err := os.Chmod(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := os.Lstat(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		ops  runtimeRootIO
	}{
		{name: "lstat", ops: runtimeRootIO{lstat: func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }}},
		{name: "unsafe before", ops: runtimeRootIO{lstat: func(string) (os.FileInfo, error) { return transactionRegularInfo(t), nil }}},
		{name: "open", ops: runtimeRootIO{lstat: func(string) (os.FileInfo, error) { return good, nil }, open: func(string) (topologyRoot, error) { return nil, errors.New("open") }}},
		{name: "stat", ops: runtimeRootIO{lstat: func(string) (os.FileInfo, error) { return good, nil }, open: func(string) (topologyRoot, error) { return &fakeTopologyRoot{statErr: errors.New("stat")}, nil }}},
		{name: "changed", ops: runtimeRootIO{lstat: func(string) (os.FileInfo, error) { return good, nil }, open: func(string) (topologyRoot, error) { return &fakeTopologyRoot{statInfo: other}, nil }}},
		{name: "unsafe after", ops: runtimeRootIO{lstat: func(string) (os.FileInfo, error) { return good, nil }, open: func(string) (topologyRoot, error) { return &fakeTopologyRoot{statInfo: transactionRegularInfo(t)}, nil }}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opened, err := openRuntimeRootWith(test.ops, activePath)
			if err == nil || opened != nil {
				t.Fatalf("open result = (%v, %v)", opened, err)
			}
		})
	}
	if opened, err := defaultRuntimeRootIO().open(filepath.Join(root, "missing")); err == nil || opened != nil {
		t.Fatalf("default missing root = (%v, %v)", opened, err)
	}
}

func TestTopologyTransactionValidationCoversEachIdentityShape(t *testing.T) {
	valid := TopologyTransactionRecord{Generation: "g", RequestID: "r", Action: "a", Target: "t", Panes: []TopologyPane{{PaneID: "p", NativeSessionID: "s", ProfileID: "profile", WorkspaceID: "w"}}}
	if canonical, err := validateTopologyRecord(valid); err != nil || canonical.SchemaVersion != 1 {
		t.Fatalf("valid record = (%+v, %v)", canonical, err)
	}
	mutations := []func(*TopologyTransactionRecord){
		func(record *TopologyTransactionRecord) { record.Generation = "" },
		func(record *TopologyTransactionRecord) { record.RequestID = "" },
		func(record *TopologyTransactionRecord) { record.Action = "" },
		func(record *TopologyTransactionRecord) { record.Target = " bad" },
		func(record *TopologyTransactionRecord) { record.Panes[0].PaneID = "" },
		func(record *TopologyTransactionRecord) { record.Panes[0].NativeSessionID = "" },
		func(record *TopologyTransactionRecord) { record.Panes[0].ProfileID = "" },
		func(record *TopologyTransactionRecord) { record.Panes[0].WorkspaceID = " bad" },
	}
	for index, mutate := range mutations {
		record := valid
		record.Panes = append([]TopologyPane(nil), valid.Panes...)
		mutate(&record)
		if _, err := validateTopologyRecord(record); err == nil {
			t.Fatalf("invalid identity %d was accepted", index)
		}
	}
	for _, value := range []string{"", strings.Repeat("x", 513), " spaced ", "line\n", "delete\x7f"} {
		if validTransactionValue(value, true) {
			t.Fatalf("invalid transaction value accepted: %q", value)
		}
	}
	if !validTransactionValue("", false) || !validTransactionValue("valid", true) {
		t.Fatal("valid transaction values were rejected")
	}
}

type fakeTopologyRoot struct {
	file             topologyFile
	openFileErr      error
	directory        topologyDirectory
	openDirectoryErr error
	lstatInfo        os.FileInfo
	lstatErr         error
	removeErr        error
	statInfo         os.FileInfo
	statErr          error
	closeErr         error
}

func (r *fakeTopologyRoot) OpenFile(string, int, os.FileMode) (topologyFile, error) {
	return r.file, r.openFileErr
}
func (r *fakeTopologyRoot) OpenDirectory(string) (topologyDirectory, error) {
	return r.directory, r.openDirectoryErr
}
func (r *fakeTopologyRoot) Lstat(string) (os.FileInfo, error) { return r.lstatInfo, r.lstatErr }
func (r *fakeTopologyRoot) Remove(string) error               { return r.removeErr }
func (r *fakeTopologyRoot) Stat(string) (os.FileInfo, error)  { return r.statInfo, r.statErr }
func (r *fakeTopologyRoot) Close() error                      { return r.closeErr }

type fakeTopologyFile struct {
	info       os.FileInfo
	statErr    error
	writeErr   error
	shortWrite bool
	syncErr    error
	closeErr   error
}

func (f *fakeTopologyFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.shortWrite {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (f *fakeTopologyFile) Stat() (os.FileInfo, error) { return f.info, f.statErr }
func (f *fakeTopologyFile) Sync() error                { return f.syncErr }
func (f *fakeTopologyFile) Close() error               { return f.closeErr }

type fakeTopologyDirectory struct {
	syncErr  error
	closeErr error
}

func (d *fakeTopologyDirectory) Sync() error  { return d.syncErr }
func (d *fakeTopologyDirectory) Close() error { return d.closeErr }

func transactionRegularInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func transactionDirectoryInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func transactionFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "sessions", "g1")
	if err := os.MkdirAll(session, 0o700); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(root, "active-runtime.json")
	data := fmt.Sprintf(`{"schemaVersion":1,"generation":"g1","sessionName":"g1","socketPath":%q,"expectedInventoryPath":%q}`,
		filepath.Join(session, "herdr.sock"), filepath.Join(session, "expected-inventory.json"))
	if err := os.WriteFile(activePath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, activePath
}
