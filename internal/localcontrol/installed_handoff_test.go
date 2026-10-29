//go:build linux || darwin

package localcontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"golang.org/x/sys/unix"
)

// Only parent-supplied canonical short TMPDIR is used. No server helper, core,
// app constructor, listener, environment loader or external operation is used.
// Parent retains umask077; this fixture never changes the process umask.
func handoffFixture(t *testing.T) (config.InstalledConfig, InstalledCredentials, string) {
	t.Helper()
	base := os.Getenv("TMPDIR")
	canonical, e := filepath.EvalSymlinks(base)
	if e != nil || base == "" || canonical != base || !filepath.IsAbs(base) || len(base) > 35 {
		t.Fatal("requires supplied canonical short TMPDIR")
	}
	root, e := os.MkdirTemp(base, "ih-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := os.RemoveAll(root); e != nil {
			t.Error("private fixture cleanup failed")
		}
	})
	cfg := config.Config{Transport: config.TransportTailscaleCLI, Host: "127.0.0.1", Token: strings.Repeat("x", 32), Port: 8375, PluginPort: 8376, InstanceID: "handoff-fixture", TailscaleCLIScope: "production", TailscaleCLIBin: "/inert/tailscale", TailscaleCLIOrigin: "https://relay.example.invalid", PhoneAppOrigin: "https://app.example.invalid", TailscaleCLIStateRoot: root + "/state", TailscaleCLICoordinationRoot: root + "/coord", ConfigHome: root + "/config", RuntimeDir: root + "/r", CacheDir: root + "/cache", ReleaseRoot: root + "/release"}
	if e := os.Mkdir(cfg.RuntimeDir, 0700); e != nil {
		t.Fatal(e)
	}
	selected, e := config.BindInstalledCLI(&cfg)
	if e != nil {
		t.Fatal("fixture binding refused")
	}
	return selected, InstalledCredentials{strings.Repeat("a", 32), cfg.InstanceID, strings.Repeat("b", 64)}, cfg.RuntimeDir
}
func handoffPrepared(t *testing.T, selected config.InstalledConfig, c InstalledCredentials) InstalledHandoff {
	t.Helper()
	h, e := PrepareInstalledHandoff(context.Background(), selected, c)
	t.Cleanup(func() { _ = h.Close() })
	if e != nil {
		t.Fatal("prepare refused")
	}
	return h
}
func handoffPut(t *testing.T, path string, data []byte) {
	t.Helper()
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
}
func handoffExists(t *testing.T, path string) {
	t.Helper()
	if _, e := os.Lstat(path); e != nil {
		t.Fatal("expected preserved artifact")
	}
}

func TestInstalledHandoffCodec(t *testing.T) {
	good := `{"protocol":"installed-control-v1","installation":"fixture","run":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	cases := map[string]string{
		"valid": good, "duplicate": strings.Replace(good, `"protocol":`, `"protocol":"x","protocol":`, 1),
		"case": strings.Replace(good, "protocol", "Protocol", 1), "unknown": strings.Replace(good, "protocol", "other", 1),
		"trailing": good + ` {}`, "type": strings.Replace(good, `"fixture"`, `1`, 1), "null": strings.Replace(good, `"fixture"`, `null`, 1),
		"missing": `{"protocol":"installed-control-v1","run":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "oversize": strings.Repeat(" ", handoffBytes) + good,
		"array": `[]`, "invalid-utf8": string([]byte{0xff}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			var l handoffLocator
			e := handoffDecode([]byte(data), &l)
			if (e == nil) != (name == "valid") {
				t.Fatal("codec decision")
			}
		})
	}
	t.Run("schema", func(t *testing.T) {
		for _, r := range []string{"", strings.Repeat("A", 32), strings.Repeat("a", 31), "../s"} {
			if handoffLocatorOK(handoffLocator{InstalledProtocol, "fixture", r}, "fixture") {
				t.Fatal("invalid run accepted")
			}
		}
		if handoffLocatorOK(handoffLocator{InstalledProtocol, "other", strings.Repeat("a", 32)}, "fixture") {
			t.Fatal("wrong installation")
		}
	})
	t.Run("redaction", func(t *testing.T) {
		c := InstalledCredentials{strings.Repeat("a", 32), "fixture", strings.Repeat("b", 64)}
		values := []any{InstalledHandoffSnapshot{c, "/secret"}, handoffCredential{InstalledProtocol, c.Installation, c.Run, c.Capability}, InstalledHandoff{}}
		for _, v := range values {
			out := fmt.Sprintf("%v %+v %#v", v, v, v)
			if strings.Contains(out, c.Capability) || strings.Contains(out, "/secret") {
				t.Fatal("secret formatting")
			}
		}
	})
}
func TestInstalledHandoffPolicy(t *testing.T) {
	uid := uint32(os.Geteuid())
	for _, tc := range []struct {
		name                string
		mode                uint32
		uid                 uint32
		links               uint64
		dir, ancestor, want bool
	}{
		{"private-dir", unix.S_IFDIR | 0700, uid, 2, true, false, true}, {"readonly-dir", unix.S_IFDIR | 0500, uid, 2, true, false, false},
		{"private-file", unix.S_IFREG | 0600, uid, 1, false, false, true}, {"readonly-file", unix.S_IFREG | 0400, uid, 1, false, false, false},
		{"hardlink", unix.S_IFREG | 0600, uid, 2, false, false, false}, {"foreign", unix.S_IFREG | 0600, uid + 1, 1, false, false, false},
		{"special", unix.S_IFIFO | 0600, uid, 1, false, false, false}, {"symlink", unix.S_IFLNK | 0600, uid, 1, false, false, false},
		{"setid", unix.S_IFDIR | 04700, uid, 2, true, false, false}, {"sticky-root", unix.S_IFDIR | 01777, 0, 2, true, true, true},
		{"writable-ancestor", unix.S_IFDIR | 0777, uid, 2, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s unix.Stat_t
			handoffTestMode(&s, tc.mode)
			s.Uid = tc.uid
			handoffTestLinks(&s, tc.links)
			if handoffMode(s, tc.dir, tc.ancestor) != tc.want {
				t.Fatal("synthetic stat policy")
			}
		})
	}
	t.Run("stable-directory-identity", func(t *testing.T) {
		a := unix.Stat_t{}
		b := a
		handoffTestLinks(&b, 9)
		if handoffID(a) != handoffID(b) {
			t.Fatal("mutable links used as identity")
		}
	})
}
func TestInstalledHandoffLifecycle(t *testing.T) {
	selected, c, root := handoffFixture(t)
	ctx := context.Background()
	h := handoffPrepared(t, selected, c)
	alias := h
	path, e := h.SocketPath(ctx)
	if e != nil || path != root+"/ic/g"+c.Run+"/s" {
		t.Fatal("socket path")
	}
	if _, e := ReadInstalledHandoff(ctx, selected); e == nil {
		t.Fatal("premature discovery")
	}
	outcome, e := h.PublishDiscovery(ctx)
	if e != nil || outcome != InstalledHandoffPublished {
		t.Fatal("publish")
	}
	read, e := ReadInstalledHandoff(ctx, selected)
	if e != nil || read.Credentials() != c || read.SocketPath() != path {
		t.Fatal("strict read")
	}
	if e := h.Close(); e != nil {
		t.Fatal("close")
	}
	if _, e := alias.SocketPath(ctx); e == nil {
		t.Fatal("closed alias")
	}
	if _, e := ReadInstalledHandoff(ctx, selected); e == nil {
		t.Fatal("retired discovery")
	}
	handoffExists(t, root+"/ic/owner")
	c.Run = strings.Repeat("c", 32)
	next := handoffPrepared(t, selected, c)
	newer, e := next.SocketPath(ctx)
	if e != nil || newer == path {
		t.Fatal("restart reused socket")
	}
	if e := next.Close(); e != nil {
		t.Fatal("restart cleanup")
	}
}
func TestInstalledHandoffRefusals(t *testing.T) {
	for _, name := range []string{"missing-parent", "long-path", "wrong-installation", "same-run", "binding-drift", "cancelled", "zero"} {
		t.Run(name, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			ctx := context.Background()
			switch name {
			case "missing-parent":
				if e := os.Remove(root); e != nil {
					t.Fatal(e)
				}
			case "long-path":
				cfg := selected.Snapshot()
				cfg.RuntimeDir = filepath.Join(filepath.Dir(root), strings.Repeat("r", 80))
				if e := os.Mkdir(cfg.RuntimeDir, 0700); e != nil {
					t.Fatal(e)
				}
				var e error
				selected, e = config.BindInstalledCLI(&cfg)
				if e != nil {
					t.Fatal("binding")
				}
			case "wrong-installation":
				c.Installation = "other"
			case "same-run":
				_ = handoffPrepared(t, selected, c)
			case "binding-drift":
				h := handoffPrepared(t, selected, c)
				if e := os.Rename(root, root+"-old"); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(root, 0700); e != nil {
					t.Fatal(e)
				}
				if _, e := h.SocketPath(ctx); e == nil {
					t.Fatal("binding drift accepted")
				}
				if h.Close() == nil {
					t.Fatal("unsafe cleanup")
				}
				return
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "zero":
				selected = config.InstalledConfig{}
			}
			h, e := PrepareInstalledHandoff(ctx, selected, c)
			defer h.Close()
			if e == nil {
				t.Fatal("expected refusal")
			}
		})
	}
}
func TestInstalledHandoffPrivateFiles(t *testing.T) {
	for _, name := range []string{"mode", "symlink", "hardlink", "directory", "foreign-replacement", "malformed-current", "unknown-socket", "owner-drift"} {
		t.Run(name, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			h := handoffPrepared(t, selected, c)
			ctx := context.Background()
			if _, e := h.PublishDiscovery(ctx); e != nil {
				t.Fatal("publish")
			}
			file := root + "/ic/g" + c.Run + "/c"
			switch name {
			case "mode":
				if e := os.Chmod(file, 0640); e != nil {
					t.Fatal(e)
				} // Own synthetic wrong-mode fixture only.
			case "symlink":
				if e := os.Rename(file, file+"-old"); e != nil {
					t.Fatal(e)
				}
				if e := os.Symlink("c-old", file); e != nil {
					t.Fatal(e)
				}
			case "hardlink":
				if e := os.Link(file, file+"-link"); e != nil {
					t.Fatal(e)
				}
			case "directory":
				if e := os.Remove(file); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(file, 0700); e != nil {
					t.Fatal(e)
				}
			case "foreign-replacement":
				if e := os.Rename(file, file+"-old"); e != nil {
					t.Fatal(e)
				}
				handoffPut(t, file, []byte(`{}`))
			case "malformed-current":
				file = root + "/ic/current"
				handoffPut(t, file, []byte(`{"run":"bad"}`))
			case "unknown-socket":
				file = root + "/ic/g" + c.Run + "/s"
				handoffPut(t, file, []byte("not a socket"))
				if e := h.Close(); e == nil {
					t.Fatal("unknown leaf silently removed")
				}
				handoffExists(t, file)
				return
			case "owner-drift":
				file = root + "/ic/owner"
				handoffPut(t, file, []byte(`{}`))
			}
			if _, e := ReadInstalledHandoff(ctx, selected); e == nil {
				t.Fatal("unsafe read")
			}
			_ = h.Close()
			if name != "malformed-current" {
				handoffExists(t, file)
			}
		})
	}
}
func TestInstalledHandoffFaultWindows(t *testing.T) {
	for _, stage := range []string{"owner-before-create", "owner-write", "owner-sync", "owner-closed", "init-directory-sync", "init-rename", "init-parent-sync", "init-complete", "generation-create", "credential-before-create", "credential-write", "credential-sync", "credential-closed", "generation-sync", "prepared"} {
		t.Run(stage, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			h, e := prepareInstalledHandoff(context.Background(), selected, c, func(p string) error {
				if p == stage {
					return errHandoff
				}
				return nil
			})
			t.Cleanup(func() { _ = h.Close() })
			if e == nil {
				_ = h.Close()
				t.Fatal("fault not observed")
			}
			if (stage == "init-parent-sync" || stage == "init-complete") && !errors.Is(e, errHandoffUncertain) {
				t.Fatal("lost initialization uncertainty")
			}
			if _, e := ReadInstalledHandoff(context.Background(), selected); e == nil {
				t.Fatal("init exposed discovery")
			}
			_ = h.Close()
			if stage == "init-parent-sync" || stage == "init-complete" {
				handoffExists(t, root+"/ic/owner")
			}
		})
	}
	for _, stage := range []string{"current-before-create", "current-write", "current-sync", "current-closed", "current-rename", "current-directory-sync", "current-complete", "publication-return"} {
		t.Run(stage, func(t *testing.T) {
			selected, c, _ := handoffFixture(t)
			h := handoffPrepared(t, selected, c)
			h.state.fault = func(p string) error {
				if p == stage {
					return errHandoff
				}
				return nil
			}
			outcome, e := h.PublishDiscovery(context.Background())
			if e == nil {
				t.Fatal("fault not observed")
			}
			post := stage == "current-directory-sync" || stage == "current-complete" || stage == "publication-return"
			if post && outcome != InstalledHandoffUncertain || !post && outcome != InstalledHandoffUnpublished {
				t.Fatal("wrong outcome")
			}
			_, re := ReadInstalledHandoff(context.Background(), selected)
			if (re == nil) != post {
				t.Fatal("wrong rename window")
			}
			if e := h.Close(); e != nil {
				t.Fatal("fault cleanup")
			}
		})
	}
}
func TestInstalledHandoffBoundsAndLeftovers(t *testing.T) {
	for _, name := range []string{"staging-budget", "temporary-budget", "scan-bound", "old-generation", "old-current", "exclusive-init", "exclusive-current"} {
		t.Run(name, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			ctx := context.Background()
			if name == "staging-budget" || name == "scan-bound" {
				n := 16
				prefix := ".ic-"
				if name == "scan-bound" {
					n = 257
					prefix = "unrelated-"
				}
				for i := 0; i < n; i++ {
					handoffPut(t, fmt.Sprintf("%s/%s%d", root, prefix, i), nil)
				}
				h, e := PrepareInstalledHandoff(ctx, selected, c)
				defer h.Close()
				if e == nil {
					t.Fatal("allocation bound")
				}
				handoffExists(t, fmt.Sprintf("%s/%s0", root, prefix))
				return
			}
			if name == "exclusive-init" {
				h, e := prepareInstalledHandoff(ctx, selected, c, func(p string) error {
					if p == "init-rename" {
						return os.Mkdir(root+"/ic", 0700)
					}
					return nil
				})
				defer h.Close()
				if e == nil {
					t.Fatal("exclusive init overwrote")
				}
				handoffExists(t, root+"/ic")
				return
			}
			h := handoffPrepared(t, selected, c)
			switch name {
			case "temporary-budget":
				for i := 0; i < 16; i++ {
					handoffPut(t, fmt.Sprintf("%s/ic/t%d", root, i), nil)
				}
				if _, e := h.PublishDiscovery(ctx); e == nil {
					t.Fatal("temporary budget")
				}
			case "old-generation":
				old := root + "/ic/g" + strings.Repeat("d", 32)
				if e := os.Mkdir(old, 0700); e != nil {
					t.Fatal(e)
				}
				handoffPut(t, old+"/s", nil)
				if e := h.Close(); e != nil {
					t.Fatal("own cleanup")
				}
				handoffExists(t, old+"/s")
			case "old-current":
				if _, e := h.PublishDiscovery(ctx); e != nil {
					t.Fatal("first publication")
				}
				c.Run = strings.Repeat("e", 32)
				next := handoffPrepared(t, selected, c)
				if _, e := next.PublishDiscovery(ctx); e != nil {
					t.Fatal("replacement")
				}
				if h.Close() == nil {
					t.Fatal("replaced identity not reported")
				}
				snap, e := ReadInstalledHandoff(ctx, selected)
				if e != nil || snap.Credentials() != c {
					t.Fatal("replacement damaged")
				}
			case "exclusive-current":
				h.state.fault = func(p string) error {
					if p == "current-rename" {
						handoffPut(t, root+"/ic/current", []byte(`{}`))
					}
					return nil
				}
				out, e := h.PublishDiscovery(ctx)
				if e == nil || out != InstalledHandoffUnpublished {
					t.Fatal("exclusive current overwrote")
				}
				handoffExists(t, root+"/ic/current")
			}
		})
	}
}
func TestInstalledHandoffCancellationAndDescriptors(t *testing.T) {
	selected, c, _ := handoffFixture(t)
	h := handoffPrepared(t, selected, c)
	ctx, cancel := context.WithCancel(context.Background())
	h.state.fault = func(p string) error {
		if p == "current-directory-sync" {
			cancel()
		}
		return nil
	}
	out, e := h.PublishDiscovery(ctx)
	if e == nil || out != InstalledHandoffUncertain {
		t.Fatal("post rename cancellation")
	}
	fds := []int{h.state.root, h.state.ns, h.state.gen}
	if e := h.Close(); e != nil {
		t.Fatal("close")
	}
	for _, fd := range fds {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != unix.EBADF {
			t.Fatal("descriptor retained")
		}
	}
	if _, e := h.SocketPath(context.Background()); e == nil {
		t.Fatal("retired alias")
	}
}

// Simulate process loss without a process: close retained descriptors but leave
// on-disk artifacts. This private test helper cannot authorize production paths.
func handoffAbandon(t *testing.T, h InstalledHandoff) {
	t.Helper()
	s := h.state
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.retired.Store(true)
		close(s.done)
		for _, fd := range []int{s.gen, s.ns, s.root} {
			if fd >= 0 && unix.Close(fd) != nil {
				t.Error("crash-fixture descriptor close")
			}
		}
	})
}
func TestInstalledHandoffCrashRestart(t *testing.T) {
	for _, stage := range []string{"init-rename", "init-parent-sync", "credential-sync", "current-directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			ctx := context.Background()
			h, e := prepareInstalledHandoff(ctx, selected, c, func(p string) error {
				if p == stage {
					return errHandoff
				}
				return nil
			})
			t.Cleanup(func() { _ = h.Close() })
			if stage == "current-directory-sync" {
				if e != nil {
					t.Fatal("prepare")
				}
				out, pe := h.PublishDiscovery(ctx)
				if pe == nil || out != InstalledHandoffUncertain {
					t.Fatal("publication fault")
				}
			} else if e == nil {
				_ = h.Close()
				t.Fatal("initialization fault")
			}
			handoffAbandon(t, h)
			leftover := root + "/ic"
			if stage == "init-rename" {
				leftover = root + "/.ic-" + c.Run
			} else if stage == "credential-sync" || stage == "current-directory-sync" {
				leftover += "/g" + c.Run
			}
			handoffExists(t, leftover)
			c.Run = strings.Repeat("f", 32)
			next := handoffPrepared(t, selected, c)
			if _, e := next.PublishDiscovery(ctx); e != nil {
				t.Fatal("fresh generation publication")
			}
			snap, e := ReadInstalledHandoff(ctx, selected)
			if e != nil || snap.Credentials() != c {
				t.Fatal("fresh generation read")
			}
			if e := next.Close(); e != nil {
				t.Fatal("fresh generation cleanup")
			}
			handoffExists(t, leftover)
		})
	}
}

func TestInstalledHandoffRetirementDrain(t *testing.T) {
	selected, c, _ := handoffFixture(t)
	h := handoffPrepared(t, selected, c)
	alias := h
	entered, release, finished, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	h.state.fault = func(p string) error {
		if p == "socket-path" {
			close(entered)
			<-release
		}
		return nil
	}
	go func() { defer close(finished); _, _ = alias.SocketPath(context.Background()) }()
	// Every failure path unblocks and joins both operations before fixture cleanup.
	defer func() { unblock(); <-finished }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not enter")
	}
	go func() { defer close(closed); _ = h.Close() }()
	defer func() { unblock(); <-closed }()
	select {
	case <-h.state.done:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement not signalled")
	}
	select {
	case <-closed:
		t.Fatal("close failed to drain")
	default:
	}
	if _, e := alias.SocketPath(context.Background()); e == nil {
		t.Fatal("retired copy admitted")
	}
	unblock()
	<-finished
	<-closed
}

func TestInstalledHandoffReplacementAndCancellation(t *testing.T) {
	for _, name := range []string{"invalid-old-current", "replaced-temp", "post-rename-cleanup-error", "queued-cancel", "immutable-credential", "wrong-run-record"} {
		t.Run(name, func(t *testing.T) {
			selected, c, root := handoffFixture(t)
			h := handoffPrepared(t, selected, c)
			ctx := context.Background()
			switch name {
			case "invalid-old-current":
				handoffPut(t, root+"/ic/current", []byte(`{}`))
				out, e := h.PublishDiscovery(ctx)
				if e == nil || out != InstalledHandoffUnpublished {
					t.Fatal("invalid old current replaced")
				}
				handoffExists(t, root+"/ic/current")
			case "replaced-temp":
				h.state.fault = func(p string) error {
					if p == "current-rename" {
						name := root + "/ic/t" + c.Run
						if e := os.Rename(name, name+"-old"); e != nil {
							return e
						}
						handoffPut(t, name, []byte(`{}`))
					}
					return nil
				}
				out, e := h.PublishDiscovery(ctx)
				if e == nil || out != InstalledHandoffUnpublished {
					t.Fatal("foreign temp published")
				}
				if h.Close() == nil {
					t.Fatal("foreign temp not reported")
				}
				handoffExists(t, root+"/ic/t"+c.Run)
			case "post-rename-cleanup-error":
				h.state.fault = func(p string) error {
					if p == "current-directory-sync" {
						handoffPut(t, root+"/ic/g"+c.Run+"/s", nil)
						return errHandoff
					}
					return nil
				}
				out, primary := h.PublishDiscovery(ctx)
				cleanup := h.Close()
				combined := errors.Join(primary, cleanup)
				if out != InstalledHandoffUncertain || !errors.Is(combined, errHandoffUncertain) || !errors.Is(combined, errHandoffCleanup) {
					t.Fatal("lost primary uncertainty or cleanup error")
				}
			case "queued-cancel":
				h.state.gate <- struct{}{}
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				_, e := h.SocketPath(cancelled)
				<-h.state.gate
				if e == nil {
					t.Fatal("queued cancellation")
				}
			case "immutable-credential", "wrong-run-record":
				if _, e := h.PublishDiscovery(ctx); e != nil {
					t.Fatal("publish")
				}
				record := h.state.record()
				if name == "immutable-credential" {
					record.Capability = strings.Repeat("d", 64)
				} else {
					record.Run = strings.Repeat("d", 32)
				}
				data, _ := json.Marshal(record)
				handoffPut(t, root+"/ic/g"+c.Run+"/c", data)
				if _, e := h.SocketPath(ctx); e == nil {
					t.Fatal("immutable record drift")
				}
				if name == "wrong-run-record" {
					if _, e := ReadInstalledHandoff(ctx, selected); e == nil {
						t.Fatal("locator credential mismatch")
					}
				}
			}
		})
	}
}

// Keep JSON encoding explicitly secret-bearing; formatting is redacted.
func TestInstalledHandoffCredentialSchema(t *testing.T) {
	c := handoffCredential{InstalledProtocol, "fixture", strings.Repeat("a", 32), strings.Repeat("b", 64)}
	data, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	var got handoffCredential
	if handoffDecode(data, &got) != nil || got != c {
		t.Fatal("credential roundtrip")
	}
}
