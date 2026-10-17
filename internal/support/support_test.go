package support

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

type forbiddenPublicMarshaler struct{ called *bool }

func (m forbiddenPublicMarshaler) MarshalJSON() ([]byte, error) {
	*m.called = true
	return nil, errors.New("private marshaler invoked")
}

func TestPublicProjectionFiniteSchemaAndNonmutation(t *testing.T) {
	called := false
	const secret = "private-projection-canary"
	s := Snapshot{
		GeneratedAt: secret, Version: secret, Revision: secret, Protocol: 3,
		ReleaseDirectory: secret, WebHash: secret, WebVersion: secret, Readiness: "ready",
		Inventory:  map[string]any{"state": "ready", secret: map[string]any{secret: forbiddenPublicMarshaler{&called}}},
		Components: map[string]string{secret: secret}, Scheduler: forbiddenPublicMarshaler{&called},
		Transport: forbiddenPublicMarshaler{&called}, UDP: forbiddenPublicMarshaler{&called},
		ActivityFailures: 7, TopologyRetries: 0, PollFailures: 2, RecentErrors: []string{secret},
	}
	p := PublicProjection(s)
	data, err := json.Marshal(p)
	if err != nil || len(data) > 4096 || called || len(s.Inventory) != 2 || len(s.Inventory[secret].(map[string]any)) != 1 {
		t.Fatal("public projection visited or changed private payload, or exceeded bound")
	}
	want := `{"schema":"support-public-v1","source":"persisted_snapshot","meaning":"historical_only_not_current_health_or_admission","protocol":3,"recorded_readiness":"ready","recorded_inventory":"ready","activity_failures":7,"topology_retries":0,"poll_failures":2,"recent_error_count":1}`
	if string(data) != want || bytes.Contains(data, []byte(secret)) {
		t.Fatal("public schema differs from exact allowed historical fields")
	}
	// Deliberately never JSON-encode s: its opaque marshalers must remain untouched.
	if s.GeneratedAt != secret || s.Components[secret] != secret || s.RecentErrors[0] != secret {
		t.Fatal("private snapshot mutated")
	}
}

func TestPublicProjectionUnknownsAndBound(t *testing.T) {
	for _, s := range []Snapshot{{}, {Protocol: -1, Readiness: "ready", PollFailures: 0},
		{Protocol: 3, Readiness: "private-canary", Inventory: map[string]any{"state": 42},
			ActivityFailures: ^uint64(0), TopologyRetries: ^uint64(0), PollFailures: -1,
			RecentErrors: make([]string, 257)}} {
		data, err := json.Marshal(PublicProjection(s))
		if err != nil || len(data) > 4096 || bytes.Contains(data, []byte("private-canary")) ||
			bytes.Contains(data, []byte(`"activity_failures"`)) || bytes.Contains(data, []byte(`"poll_failures"`)) ||
			bytes.Contains(data, []byte(`"recent_error_count"`)) ||
			!bytes.Contains(data, []byte(`"recorded_readiness":"unknown"`)) ||
			!bytes.Contains(data, []byte(`"recorded_inventory":"unknown"`)) {
			t.Fatal("invalid or missing recorded evidence was represented as valid")
		}
		if s.Protocol != 3 && bytes.Contains(data, []byte(`"protocol"`)) {
			t.Fatal("invalid protocol asserted")
		}
	}
	called := false
	p := PublicProjection(Snapshot{Protocol: 3, Inventory: map[string]any{"state": forbiddenPublicMarshaler{&called}}})
	if called || p.RecordedInventory != "unknown" || p.RecentErrorCount != nil ||
		p.ActivityFailures == nil || *p.ActivityFailures != 0 {
		t.Fatal("opaque inventory or nil errors became a claimed recorded value")
	}
	for _, state := range []string{"starting", "error"} {
		p := PublicProjection(Snapshot{Protocol: 3, Inventory: map[string]any{"state": state}})
		if p.RecordedInventory != state {
			t.Fatal("finite inventory state lost")
		}
	}
}

// Authored only: every reader/writer/race/special-file operation below is on
// disposable fixtures for later authorized hosted execution, never user state.
func supportFixture(t *testing.T) (string, []byte) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("fixture path unavailable")
	}
	// t.TempDir() makes this directory with the process umask (0755 on the hosted runners),
	// but the reader's contract is a PRIVATE selected directory; the explicit "public
	// directory" case below still chmods 0755 to prove the refusal.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal("fixture directory mode failed")
	}
	snapshot := Snapshot{
		GeneratedAt: "2001-02-03T04:05:06.123456789+02:00", Version: "1.2.3", Revision: "abc123", Protocol: 2,
		ReleaseDirectory: "/private/path-canary", WebHash: "private-hash-canary", WebVersion: "web-version",
		Readiness: "historical-ready", Inventory: map[string]any{"state": "stale", "nested": map[string]any{"items": []any{true, nil, "node-canary"}}},
		Components: map[string]string{"http": "running", "poller": "historical"},
		Scheduler:  map[string]any{"dispatched": 4, "latency_samples": 3},
		Transport:  map[string]any{"connected_by_transport": map[string]any{"websocket": 1}}, UDP: map[string]any{"received": 2},
		ActivityFailures: ^uint64(0), TopologyRetries: 42, PollFailures: 3,
		RecentErrors: []string{"secret-canary https://user:password@node-canary.invalid/\x1b[31m"},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	supportPut(t, dir, data, 0o600)
	return dir, data
}
func supportPut(t *testing.T, dir string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(Path(dir), data, mode); err != nil {
		t.Fatal("fixture write failed")
	}
	if err := os.Chmod(Path(dir), mode); err != nil {
		t.Fatal("fixture mode failed")
	}
}
func supportTree(t *testing.T, root string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&out, "%s %v %d %d\n", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, "%x\n", data)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintln(&out, target)
		}
		return nil
	})
	if err != nil {
		t.Fatal("fixture inventory unavailable")
	}
	return out.String()
}
func supportError(t *testing.T, snapshot Snapshot, err error) {
	t.Helper()
	if err != errSupportUnavailable && err != errSupportRefused && err != errSupportInvalid {
		t.Fatal("nonfinite support error")
	}
	if !reflect.DeepEqual(snapshot, Snapshot{}) || errors.Unwrap(err) != nil {
		t.Fatal("refusal returned partial data or a raw cause")
	}
	for _, output := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		for _, canary := range []string{"secret-canary", "path-canary", "node-canary", "password", "https://", "\x1b"} {
			if strings.Contains(output, canary) {
				t.Fatal("private input escaped fixed error")
			}
		}
	}
}
func supportClosed(t *testing.T, files []*os.File) {
	t.Helper()
	for _, file := range files {
		if _, err := file.Stat(); err == nil {
			t.Fatal("support content descriptor retained")
		}
	}
}

// supportDiagnostics is evidence only, never an assertion: when a positive Linux load
// fails it names the finite load error and describes each ancestor of the fixture
// directory with the loader's own predicates (supportDirectory, supportFilesystemType).
// It prints depth indexes, modes, owner classes and verdicts, never path names or ids.
func supportDiagnostics(dir string, loadErr error) string {
	var out strings.Builder
	fmt.Fprintf(&out, "load error: %v", loadErr)
	parts := append([]string{""}, strings.Split(strings.TrimPrefix(dir, "/"), "/")...)
	current := ""
	for i, part := range parts {
		if i != 0 {
			current += "/" + part
		}
		path := current
		if path == "" {
			path = "/"
		}
		var st unix.Stat_t
		if err := unix.Stat(path, &st); err != nil {
			fmt.Fprintf(&out, "; depth %d: stat failed", i)
			continue
		}
		var fsStat unix.Statfs_t
		fsKnown := unix.Statfs(path, &fsStat) == nil
		fsType := uint64(fsStat.Type)
		owner := "other"
		if st.Uid == uint32(os.Geteuid()) {
			owner = "euid"
		} else if st.Uid == 0 {
			owner = "root"
		}
		fmt.Fprintf(&out, "; depth %d: mode %04o owner %s directory-ok %t filesystem %#x ok %t",
			i, uint32(st.Mode)&0o7777, owner, supportDirectory(&st, i == len(parts)-1),
			fsType, fsKnown && supportFilesystemType(fsType))
	}
	return out.String()
}

func TestWriteAndLoadPrivateSnapshot(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("fixture path unavailable")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal("fixture directory mode failed")
	}
	snapshot := Snapshot{Version: "1.2.3", Revision: "abc123", Protocol: 2, Readiness: "ready",
		Inventory: map[string]any{"state": "ready"}, Components: map[string]string{"http": "running"}, RecentErrors: []string{"safe error"}}
	if err := Write(dir, snapshot); err != nil {
		t.Fatal("fixture writer failed")
	}
	info, err := os.Stat(Path(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("writer private mode changed")
	}
	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal("fixture read failed")
	}
	var expected Snapshot
	if json.Unmarshal(data, &expected) != nil {
		t.Fatal("writer JSON contract changed")
	}
	before := supportTree(t, dir)
	loaded, err := Load(dir)
	if runtime.GOOS != "linux" {
		supportError(t, loaded, err)
		if err != errSupportUnavailable {
			t.Fatal("native writer safety was guessed")
		}
	} else {
		if err != nil || !reflect.DeepEqual(loaded, expected) || loaded.GeneratedAt == "" {
			t.Fatalf("legitimate writer/reader round trip failed: %s", supportDiagnostics(dir, err))
		}
	}
	if before != supportTree(t, dir) {
		t.Fatal("Load changed the writer's bytes/modes/inventory")
	}
}
func TestLoadHistoricalCompletePrivateSnapshotAndAdjacentCanaries(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		t.Run(fmt.Sprint(readonly), func(t *testing.T) {
			dir, data := supportFixture(t)
			for _, name := range []string{"token", "devices.json", "journal.json", ".env", "runtime-canary"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("secret-canary"), 0o600); err != nil {
					t.Fatal("canary setup failed")
				}
			}
			if readonly {
				if err := os.Chmod(Path(dir), 0o400); err != nil {
					t.Fatal("fixture mode failed")
				}
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal("fixture mode failed")
				}
				defer os.Chmod(dir, 0o700) // disposable cleanup only, not Load
			}
			before := supportTree(t, dir)
			var expected Snapshot
			if json.Unmarshal(data, &expected) != nil {
				t.Fatal("fixture JSON invalid")
			}
			files := []*os.File{}
			reads := []int64{}
			hooks := &supportLoadHooks{event: func(stage string, file *os.File) {
				if stage == "open" {
					files = append(files, file)
				}
				if stage == "read" {
					pos, err := file.Seek(0, io.SeekCurrent)
					if err != nil {
						t.Fatal("fixture read count failed")
					}
					reads = append(reads, pos)
				}
			}}
			loaded, err := loadSnapshot(dir, hooks)
			if runtime.GOOS != "linux" {
				supportError(t, loaded, err)
				if err != errSupportUnavailable || len(files) != 0 || len(reads) != 0 {
					t.Fatal("unsupported native policy acquired content")
				}
			} else {
				if err != nil || !reflect.DeepEqual(loaded, expected) || loaded.GeneratedAt != expected.GeneratedAt {
					t.Fatal("historical snapshot or opaque metrics changed; " + supportDiagnostics(dir, err))
				}
				if !strings.Contains(loaded.RecentErrors[0], "secret-canary") {
					t.Fatal("private Snapshot was falsely scrubbed/redacted")
				}
				if len(files) != 1 || len(reads) != 2 || reads[0] != int64(len(data)) || reads[1] != int64(len(data)) {
					t.Fatal("reader opened adjacent content or omitted complete EOF")
				}
			}
			supportClosed(t, files)
			if before != supportTree(t, dir) {
				t.Fatal("reader mutated disposable data")
			}
		})
	}
}
func TestLoadUnsupportedNativePermissionModelBeforeAcquisition(t *testing.T) {
	dir, _ := supportFixture(t)
	if runtime.GOOS == "linux" {
		return
	} // actual native refusal requires the native host matrix
	before := supportTree(t, dir)
	hooks := &supportLoadHooks{event: func(string, *os.File) { t.Fatal("unsupported native Load began acquisition") }}
	loaded, err := loadSnapshot(dir, hooks)
	supportError(t, loaded, err)
	if err != errSupportUnavailable || before != supportTree(t, dir) {
		t.Fatal("native unavailability mutated or guessed private state")
	}
	if err := os.Remove(Path(dir)); err != nil {
		t.Fatal("fixture removal failed")
	}
	loaded, err = Load(dir)
	supportError(t, loaded, err)
	if err != errSupportUnavailable {
		t.Fatal("native refusal tried discovery or repair")
	}
}
func TestLoadMissingUnsafePathsSpecialFilesAndPrivateModes(t *testing.T) {
	for _, kind := range []string{"missing", "missing directory", "relative", "unclean", "control", "file symlink", "parent symlink", "ancestor symlink", "hardlink", "directory", "fifo", "socket", "public file", "writable file", "unreadable file", "special bits", "public directory", "unreadable directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			dir, data := supportFixture(t)
			if kind == "socket" {
				// Short owned fixture path avoids native Unix socket name limits.
				short, err := os.MkdirTemp("", "ss-")
				if err != nil {
					t.Fatal("short fixture unavailable")
				}
				t.Cleanup(func() { _ = os.RemoveAll(short) })
				dir, err = filepath.EvalSymlinks(short)
				if err != nil {
					t.Fatal("short fixture unavailable")
				}
				supportPut(t, dir, data, 0o600)
			}
			root := dir
			selected := dir
			switch kind {
			case "missing":
				if err := os.Remove(Path(dir)); err != nil {
					t.Fatal("fixture removal failed")
				}
			case "missing directory":
				selected = filepath.Join(dir, "absent-path-canary")
			case "relative":
				selected = "private-path-canary"
			case "unclean":
				selected = dir + "/../" + filepath.Base(dir)
			case "control":
				selected = dir + "/path-canary\x1b"
			case "file symlink", "hardlink", "directory", "fifo", "socket":
				if err := os.Rename(Path(dir), filepath.Join(dir, "private-original")); err != nil {
					t.Fatal("fixture replacement failed")
				}
				switch kind {
				case "file symlink":
					if err := os.Symlink("private-original", Path(dir)); err != nil {
						t.Fatal("fixture link failed")
					}
				case "hardlink":
					if err := os.Link(filepath.Join(dir, "private-original"), Path(dir)); err != nil {
						t.Fatal("fixture link failed")
					}
				case "directory":
					if err := os.Mkdir(Path(dir), 0o700); err != nil {
						t.Fatal("fixture directory failed")
					}
				case "fifo":
					if err := unix.Mkfifo(Path(dir), 0o600); err != nil {
						t.Fatal("fixture FIFO failed")
					}
				case "socket":
					// Owned temporary socket only; no dial, request or real service.
					listener, err := net.Listen("unix", Path(dir))
					if err != nil {
						t.Fatal("fixture socket failed")
					}
					defer listener.Close()
				}
			case "parent symlink", "ancestor symlink":
				physical := filepath.Join(dir, "physical")
				if err := os.Mkdir(physical, 0o700); err != nil {
					t.Fatal("fixture directory failed")
				}
				link := filepath.Join(dir, "alias")
				if err := os.Symlink(physical, link); err != nil {
					t.Fatal("fixture link failed")
				}
				selected = link
				if kind == "ancestor symlink" {
					if err := os.Mkdir(filepath.Join(physical, "child"), 0o700); err != nil {
						t.Fatal("fixture directory failed")
					}
					selected = filepath.Join(link, "child")
				}
			case "public file":
				if err := os.Chmod(Path(dir), 0o644); err != nil {
					t.Fatal("fixture mode failed")
				}
			case "writable file":
				if err := os.Chmod(Path(dir), 0o622); err != nil {
					t.Fatal("fixture mode failed")
				}
			case "unreadable file":
				if err := os.Chmod(Path(dir), 0); err != nil {
					t.Fatal("fixture mode failed")
				}
			case "special bits":
				if err := os.Chmod(Path(dir), 0o600|os.ModeSetuid); err != nil {
					t.Fatal("fixture mode failed")
				}
			case "public directory":
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatal("fixture mode failed")
				}
			case "unreadable directory":
				if err := os.Chmod(dir, 0o200); err != nil {
					t.Fatal("fixture mode failed")
				}
				defer os.Chmod(dir, 0o700)
			case "oversized":
				supportPut(t, dir, bytes.Repeat([]byte(" "), maxSupportBytes+1), 0o600)
			}
			// Restore unreadable fixture modes only around the independent test
			// inventory, never from Load. Root runners still exercise mode refusal.
			unreadable := kind == "unreadable file" || kind == "unreadable directory"
			if kind == "unreadable file" {
				if err := os.Chmod(Path(dir), 0o600); err != nil {
					t.Fatal("fixture mode failed")
				}
			}
			if kind == "unreadable directory" {
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal("fixture mode failed")
				}
			}
			before := supportTree(t, root)
			if kind == "unreadable file" {
				if err := os.Chmod(Path(dir), 0); err != nil {
					t.Fatal("fixture mode failed")
				}
			}
			if kind == "unreadable directory" {
				if err := os.Chmod(dir, 0o200); err != nil {
					t.Fatal("fixture mode failed")
				}
			}
			opened := false
			loaded, err := loadSnapshot(selected, &supportLoadHooks{event: func(stage string, _ *os.File) {
				if stage == "open" {
					opened = true
				}
			}})
			supportError(t, loaded, err)
			if opened {
				t.Fatal("unsafe object content was opened")
			}
			if runtime.GOOS == "linux" {
				want := errSupportRefused
				if kind == "missing" || kind == "missing directory" || kind == "oversized" {
					want = errSupportUnavailable
				}
				if kind != "unreadable directory" && err != want {
					t.Fatal("missing/unsafe/oversize finite distinction changed")
				}
			}
			if runtime.GOOS != "linux" && err != errSupportUnavailable {
				t.Fatal("native permission model inferred from modes")
			}
			if unreadable {
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal("fixture mode failed")
				}
				if kind == "unreadable file" {
					if err := os.Chmod(Path(dir), 0o600); err != nil {
						t.Fatal("fixture mode failed")
					}
				}
			}
			if before != supportTree(t, root) {
				t.Fatal("refusal mutated fixture tree")
			}
		})
	}
}
func TestSupportPrivateMetadataOwnershipAndACLMaskPolicy(t *testing.T) {
	for _, kind := range []uint64{0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x858458f6, 0x794c7630} {
		if !supportFilesystemType(kind) {
			t.Fatal("declared local POSIX filesystem unavailable")
		}
	}
	for _, kind := range []uint64{0, 0x6969, 0x65735546, 0xff534d42} { // unknown, NFS, FUSE, CIFS
		if supportFilesystemType(kind) {
			t.Fatal("unestablished filesystem permission model admitted")
		}
	}
	st := unix.Stat_t{Mode: unix.S_IFREG | 0o600, Uid: uint32(os.Geteuid()), Nlink: 1}
	if !supportRegular(&st) {
		t.Fatal("private file policy rejected")
	}
	st.Uid++
	if supportRegular(&st) {
		t.Fatal("foreign owner allowed")
	}
	st.Uid--
	st.Nlink = 2
	if supportRegular(&st) {
		t.Fatal("hardlink alias allowed")
	}
	st.Nlink = 1
	// Linux POSIX ACL mask maps to group-class bits: ANY effective foreign
	// read/write in that class is refused for this private snapshot.
	st.Mode |= 0o040
	if supportRegular(&st) {
		t.Fatal("ACL-mask foreign readability allowed")
	}
	st.Mode &^= 0o040
	st.Mode |= 0o020
	if supportRegular(&st) {
		t.Fatal("ACL-mask foreign writing allowed")
	}
	dir := unix.Stat_t{Mode: unix.S_IFDIR | 0o700, Uid: uint32(os.Geteuid())}
	if !supportDirectory(&dir, true) {
		t.Fatal("private directory rejected")
	}
	dir.Mode |= 0o010
	if supportDirectory(&dir, true) {
		t.Fatal("ACL-mask directory access allowed")
	}
	dir.Mode &^= 0o010
	dir.Uid++
	if supportDirectory(&dir, true) {
		t.Fatal("foreign private directory owner allowed")
	}
	st.Mode = unix.S_IFIFO | 0o600
	if supportRegular(&st) {
		t.Fatal("FIFO accepted as private regular input")
	}
	st.Mode = unix.S_IFCHR | 0o600
	if supportRegular(&st) {
		t.Fatal("device accepted as private regular input")
	}
}
func TestLoadChangingReplacementReadBoundsAndDescriptorClosure(t *testing.T) {
	for _, kind := range []string{"before-open replacement", "file replacement", "parent replacement", "chmod", "growth", "truncation", "read error", "oversize growth"} {
		t.Run(kind, func(t *testing.T) {
			dir, data := supportFixture(t)
			base := filepath.Dir(dir)
			files := []*os.File{}
			mutated := false
			postMutation := ""
			maximum := int64(0)
			hooks := &supportLoadHooks{event: func(stage string, file *os.File) {
				if stage == "open" {
					files = append(files, file)
				}
				if stage == "read" {
					if pos, err := file.Seek(0, io.SeekCurrent); err == nil && pos > maximum {
						maximum = pos
					}
				}
				trigger := "before-final"
				if kind == "before-open replacement" {
					trigger = "before-open"
				}
				if kind == "read error" || kind == "oversize growth" {
					trigger = "open"
				}
				if stage != trigger || mutated {
					return
				}
				mutated = true
				switch kind {
				case "before-open replacement", "file replacement":
					if err := os.Rename(Path(dir), filepath.Join(dir, "old-input")); err != nil {
						t.Fatal("fixture replacement failed")
					}
					supportPut(t, dir, data, 0o600)
				case "parent replacement":
					if err := os.Rename(dir, dir+"-old"); err != nil {
						t.Fatal("fixture parent move failed")
					}
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal("fixture parent failed")
					}
					supportPut(t, dir, data, 0o600)
				case "chmod":
					if err := os.Chmod(Path(dir), 0o400); err != nil {
						t.Fatal("fixture mode failed")
					}
				case "growth":
					supportPut(t, dir, append(data, ' '), 0o600)
				case "truncation":
					supportPut(t, dir, data[:len(data)-1], 0o600)
				case "read error":
					if err := file.Close(); err != nil {
						t.Fatal("fixture close failed")
					}
				case "oversize growth":
					supportPut(t, dir, bytes.Repeat([]byte(" "), maxSupportBytes+2*supportReadChunk), 0o600)
				}
				postMutation = supportTree(t, base)
			}}
			before := supportTree(t, base)
			loaded, err := loadSnapshot(dir, hooks)
			supportError(t, loaded, err)
			supportClosed(t, files)
			if runtime.GOOS != "linux" {
				if mutated || len(files) != 0 || before != supportTree(t, base) {
					t.Fatal("native refusal acquired or mutated")
				}
				return
			}
			if !mutated || postMutation != supportTree(t, base) {
				t.Fatal("reader changed the injected fixture state")
			}
			if maximum > maxSupportBytes+1 {
				t.Fatal("read exceeded complete limit+1 bound")
			}
			if kind == "oversize growth" && maximum != maxSupportBytes+1 {
				t.Fatal("oversize growth bound was not actually exercised")
			}
		})
	}
}
func supportRaw(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if json.Unmarshal(data, &out) != nil {
		t.Fatal("fixture projection failed")
	}
	return out
}
func supportEncode(t *testing.T, raw map[string]json.RawMessage) []byte {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	return data
}
func TestSupportStrictKnownTypesOptionalOpaqueAdditiveAndHistoricalTimestamp(t *testing.T) {
	_, base := supportFixture(t)
	for _, kind := range []string{"complete", "optional absent", "nil maps/errors", "opaque values", "additive aliases", "unicode"} {
		t.Run(kind, func(t *testing.T) {
			raw := supportRaw(t, base)
			switch kind {
			case "optional absent":
				for _, name := range []string{"release_directory", "web_hash", "web_version", "scheduler", "transport", "udp"} {
					delete(raw, name)
				}
			case "nil maps/errors":
				for _, name := range []string{"inventory", "components", "recent_errors"} {
					raw[name] = json.RawMessage("null")
				}
			case "opaque values":
				raw["scheduler"] = json.RawMessage(`[true,null,{"opaque":"retained"}]`)
				raw["transport"] = json.RawMessage(`"opaque historical transport"`)
				raw["udp"] = json.RawMessage(`false`)
			case "additive aliases":
				raw["Generated_At"] = json.RawMessage(`"secret-canary"`)
				raw["Version"] = json.RawMessage(`"secret-canary"`)
				raw["future"] = json.RawMessage(`{"array":[null,true,1,"future"]}`)
			case "unicode":
				raw["scheduler"] = json.RawMessage(`{"literal":"é","pair":"\ud83d\ude00","backslash":"\\ud800","quote":"\""}`)
			}
			loaded, err := decodeSupport(supportEncode(t, raw))
			if err != nil || loaded.GeneratedAt != "2001-02-03T04:05:06.123456789+02:00" || loaded.Version != "1.2.3" {
				t.Fatal("historical compatibility or exact-key projection changed")
			}
			encoded, err := json.Marshal(loaded)
			if err != nil || bytes.Contains(encoded, []byte("Generated_At")) || bytes.Contains(encoded, []byte("future")) {
				t.Fatal("unknown additive fields retained for output")
			}
		})
	}
	for _, name := range []string{"generated_at", "version", "revision", "protocol", "readiness", "inventory", "components", "activity_failures", "topology_retries", "poll_failures", "recent_errors"} {
		t.Run("missing "+name, func(t *testing.T) {
			raw := supportRaw(t, base)
			delete(raw, name)
			loaded, err := decodeSupport(supportEncode(t, raw))
			supportError(t, loaded, err)
			if err != errSupportInvalid {
				t.Fatal("missing required field accepted")
			}
		})
	}
	for index, field := range []struct{ name, value string }{
		{"generated_at", `"not-a-date-secret-canary"`}, {"version", "null"}, {"revision", "1"}, {"protocol", "2.0"}, {"protocol", "null"},
		{"readiness", "false"}, {"inventory", "[]"}, {"components", `{"http":null}`}, {"components", `{"http":true}`},
		{"recent_errors", `[null]`}, {"recent_errors", `[{}]`}, {"activity_failures", "-1"}, {"activity_failures", "18446744073709551616"},
		{"topology_retries", "true"}, {"poll_failures", "1.5"}, {"release_directory", "{}"}, {"web_hash", "[]"}, {"web_version", "null"},
	} {
		t.Run(fmt.Sprintf("%s/type-%d", field.name, index), func(t *testing.T) {
			raw := supportRaw(t, base)
			raw[field.name] = json.RawMessage(field.value)
			loaded, err := decodeSupport(supportEncode(t, raw))
			supportError(t, loaded, err)
			if err != errSupportInvalid {
				t.Fatal("known field type coercion accepted")
			}
		})
	}
}
func TestLoadStrictParserCapsErrorsAndNonmutation(t *testing.T) {
	for _, kind := range []string{"duplicate root", "duplicate nested", "duplicate additive", "escaped duplicate", "trailing object", "trailing secret", "invalid UTF8", "array", "null", "empty", "malformed", "depth", "object count", "total members", "array length", "string", "key", "number", "nodes", "unpaired high surrogate", "unpaired low surrogate", "invalid surrogate pair"} {
		t.Run(kind, func(t *testing.T) {
			dir, base := supportFixture(t)
			data := base
			switch kind {
			case "duplicate root":
				data = []byte(`{"version":"secret-canary",` + string(base[1:]))
			case "duplicate nested", "duplicate additive", "escaped duplicate":
				name, value := "scheduler", `{"a":1,"a":2}`
				if kind == "duplicate additive" {
					name = "future"
				}
				if kind == "escaped duplicate" {
					value = `{"a":1,"\u0061":2}`
				}
				raw := supportRaw(t, base)
				delete(raw, name)
				rest := supportEncode(t, raw)
				data = []byte(`{"` + name + `":` + value + `,` + string(rest[1:]))
			case "trailing object":
				data = append(append([]byte{}, base...), []byte(" {}")...)
			case "trailing secret":
				data = append(append([]byte{}, base...), []byte(" secret-canary")...)
			case "invalid UTF8":
				data = []byte{'{', '"', 0xff, '"', ':', '0', '}'}
			case "array":
				data = []byte("[]")
			case "null":
				data = []byte("null")
			case "empty":
				data = nil
			case "malformed":
				data = []byte(`{"generated_at":"secret-canary"`)
			default:
				value := "null"
				switch kind {
				case "depth":
					value = strings.Repeat("[", maxSupportDepth) + "null" + strings.Repeat("]", maxSupportDepth)
				case "array length":
					value = "[" + strings.Repeat("0,", maxSupportArray) + "0]"
				case "string":
					value = `"` + strings.Repeat("x", maxSupportString+1) + `"`
				case "key":
					value = `{"` + strings.Repeat("k", maxSupportKey+1) + `":0}`
				case "number":
					value = strings.Repeat("1", maxSupportNumber+1)
				case "unpaired high surrogate":
					value = `"\ud800"`
				case "unpaired low surrogate":
					value = `"\udc00"`
				case "invalid surrogate pair":
					value = `"\ud800\u0041"`
				case "object count", "total members":
					var object strings.Builder
					object.WriteByte('{')
					count := maxSupportObject + 1
					if kind == "total members" {
						count = maxSupportObject
					}
					for i := 0; i < count; i++ {
						if i > 0 {
							object.WriteByte(',')
						}
						fmt.Fprintf(&object, `"k%d":0`, i)
					}
					object.WriteByte('}')
					value = object.String()
					if kind == "total members" {
						value = "[" + strings.Repeat(value+",", maxSupportMembers/maxSupportObject) + "{}]"
					}
				case "nodes":
					value = "[" + strings.Repeat("["+strings.Repeat("0,", maxSupportArray-1)+"0],", maxSupportNodes/maxSupportArray) + "[]]"
				}
				data = []byte(`{"future":` + value + `,` + string(base[1:]))
			}
			supportPut(t, dir, data, 0o600)
			before := supportTree(t, dir)
			want := errSupportInvalid
			switch kind {
			case "depth", "object count", "total members", "array length", "string", "key", "number", "nodes":
				want = errSupportUnavailable
			}
			loaded, err := Load(dir)
			supportError(t, loaded, err)
			if runtime.GOOS == "linux" && err != want {
				t.Fatal("malformed/over-budget snapshot finite distinction changed")
			}
			if before != supportTree(t, dir) {
				t.Fatal("parser refusal changed fixture tree")
			}
			// Grammar also runs on unsupported hosts without bypassing native
			// filesystem safety. This is memory-only parsing, not Load success.
			loaded, err = decodeSupport(data)
			supportError(t, loaded, err)
			if err != want {
				t.Fatal("strict grammar/cap finite distinction changed")
			}
		})
	}
}
func TestSupportStrictJSONExactCompatibilityBudgets(t *testing.T) {
	_, base := supportFixture(t)
	for _, kind := range []string{"string", "key", "number", "array", "object", "depth"} {
		t.Run(kind, func(t *testing.T) {
			value := "null"
			switch kind {
			case "string":
				value = `"` + strings.Repeat("x", maxSupportString) + `"`
			case "key":
				value = `{"` + strings.Repeat("x", maxSupportKey) + `":0}`
			case "number":
				value = strings.Repeat("1", maxSupportNumber)
			case "array":
				value = "[" + strings.Repeat("0,", maxSupportArray-1) + "0]"
			case "object":
				var out strings.Builder
				out.WriteByte('{')
				for i := 0; i < maxSupportObject; i++ {
					if i > 0 {
						out.WriteByte(',')
					}
					fmt.Fprintf(&out, `"k%d":0`, i)
				}
				out.WriteByte('}')
				value = out.String()
			case "depth":
				value = strings.Repeat("[", maxSupportDepth-2) + "null" + strings.Repeat("]", maxSupportDepth-2)
			}
			data := []byte(`{"future":` + value + `,` + string(base[1:]))
			loaded, err := decodeSupport(data)
			if err != nil || loaded.GeneratedAt != "2001-02-03T04:05:06.123456789+02:00" {
				t.Fatal("exact safe additive compatibility budget rejected")
			}
		})
	}
}
func TestLoadCompleteByteBoundaryAndLimitPlusOne(t *testing.T) {
	for _, kind := range []string{"exact", "one over", "valid prefix huge whitespace", "valid prefix suffix", "multiple chunks"} {
		t.Run(kind, func(t *testing.T) {
			dir, base := supportFixture(t)
			size := maxSupportBytes
			if kind == "one over" {
				size++
			}
			if kind == "valid prefix huge whitespace" {
				size *= 2
			}
			if kind == "multiple chunks" {
				size = 2*supportReadChunk + 1
			}
			data := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), size-len(base))...)
			if kind == "valid prefix suffix" {
				data[len(data)-1] = 'x'
			}
			supportPut(t, dir, data, 0o600)
			before := supportTree(t, dir)
			files := []*os.File{}
			positions := []int64{}
			loaded, err := loadSnapshot(dir, &supportLoadHooks{event: func(stage string, file *os.File) {
				if stage == "open" {
					files = append(files, file)
				}
				if stage == "read" {
					pos, err := file.Seek(0, io.SeekCurrent)
					if err != nil {
						t.Fatal("read position unavailable")
					}
					positions = append(positions, pos)
				}
			}})
			if runtime.GOOS != "linux" {
				supportError(t, loaded, err)
				if len(files) != 0 || err != errSupportUnavailable {
					t.Fatal("native unavailable policy read input")
				}
			} else {
				switch kind {
				case "one over", "valid prefix huge whitespace":
					supportError(t, loaded, err)
					if err != errSupportUnavailable || len(files) != 0 {
						t.Fatal("oversized named file opened or prefix accepted")
					}
				case "valid prefix suffix":
					supportError(t, loaded, err)
					if err != errSupportInvalid {
						t.Fatal("complete suffix not checked")
					}
				default:
					if err != nil || loaded.GeneratedAt == "" || len(files) != 1 || positions[len(positions)-1] != int64(size) {
						t.Fatal("exact complete boundary rejected or truncated")
					}
				}
			}
			for i, pos := range positions {
				if pos > maxSupportBytes+1 || (i > 0 && pos-positions[i-1] > supportReadChunk) {
					t.Fatal("streaming byte/chunk bound exceeded")
				}
			}
			supportClosed(t, files)
			if before != supportTree(t, dir) {
				t.Fatal("bounded read changed fixture")
			}
		})
	}
}
