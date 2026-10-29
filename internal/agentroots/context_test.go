package agentroots

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every dependency is logical. Unknown operands are counted and denied, never
// forwarded to process environment, filesystem, or wall clock.
type rootFixture struct {
	d          rootDependencies
	env        map[string]string
	stats      map[string]os.FileInfo
	statErrors map[string]error
	reads      map[string][]os.DirEntry
	readErrors map[string]error
	times      []time.Time
	calls      []string
	denied     int
	locked     int
	after      func(string)
}

func newRootFixture() *rootFixture {
	f := &rootFixture{
		env: make(map[string]string), stats: make(map[string]os.FileInfo),
		statErrors: make(map[string]error), reads: make(map[string][]os.DirEntry),
		readErrors: make(map[string]error),
	}
	for _, key := range []string{ClaudeListEnv, QoderListEnv, CodexListEnv, CursorListEnv,
		OpenCodeListEnv, HermesListEnv, PiListEnv, OMPListEnv, OMOListEnv,
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "HERMES_HOME", "XDG_DATA_HOME",
		"PI_CODING_AGENT_DIR", "OMO_CODING_AGENT_DIR", "SENPI_CODING_AGENT_DIR"} {
		f.env[key] = ""
	}
	f.d.cache = &profileCacheState{entries: make(map[string]profileCacheEntry)}
	f.d.getenv = func(key string) string {
		value, ok := f.env[key]
		if !ok {
			f.denied++
		}
		f.notice("env:" + key)
		return value
	}
	f.d.stat = func(path string) (os.FileInfo, error) {
		info, ok := f.stats[path]
		err, failed := f.statErrors[path]
		if !ok && !failed {
			f.denied++
			err = os.ErrPermission
		}
		f.notice("stat:" + path)
		return info, err
	}
	f.d.readDir = func(path string) ([]os.DirEntry, error) {
		entries, ok := f.reads[path]
		err, failed := f.readErrors[path]
		if !ok && !failed {
			f.denied++
			err = os.ErrPermission
		}
		f.notice("read:" + path)
		return entries, err
	}
	f.d.now = func() time.Time {
		var now time.Time
		if len(f.times) == 0 {
			f.denied++
		} else {
			now, f.times = f.times[0], f.times[1:]
		}
		f.notice("clock")
		return now
	}
	return f
}

func (f *rootFixture) notice(call string) {
	if f.d.cache.TryLock() {
		f.d.cache.Unlock()
	} else {
		f.locked++
	}
	f.calls = append(f.calls, call)
	if f.after != nil {
		f.after(call)
	}
}

func (f *rootFixture) check(t *testing.T) {
	t.Helper()
	if f.denied != 0 || f.locked != 0 {
		t.Errorf("denied=%d callbacks under lock=%d calls=%v", f.denied, f.locked, f.calls)
	}
}

type rootInfo struct {
	name  string
	mode  os.FileMode
	stamp int64
	size  int64
}

func (i rootInfo) Name() string       { return i.name }
func (i rootInfo) Size() int64        { return i.size }
func (i rootInfo) Mode() os.FileMode  { return i.mode }
func (i rootInfo) ModTime() time.Time { return time.Unix(0, i.stamp) }
func (i rootInfo) IsDir() bool        { return i.mode.IsDir() }
func (i rootInfo) Sys() any           { return nil }

type rootEntry struct {
	name   string
	mode   os.FileMode
	onName func()
}

func (e rootEntry) Name() string {
	if e.onName != nil {
		e.onName()
	}
	return e.name
}
func (e rootEntry) IsDir() bool                { return e.mode.IsDir() }
func (e rootEntry) Type() os.FileMode          { return e.mode.Type() }
func (e rootEntry) Info() (os.FileInfo, error) { return nil, os.ErrPermission }

var contextRootAPIs = []struct {
	name string
	fn   func(context.Context, string) ([]string, error)
}{
	{"Claude", ClaudeContext}, {"Qoder", QoderContext}, {"Codex", CodexContext},
	{"Cursor", CursorContext}, {"OpenCodeData", OpenCodeDataContext},
	{"OpenCodeDBs", OpenCodeDBsContext}, {"HermesData", HermesDataContext},
	{"HermesDBs", HermesDBsContext}, {"Pi", PiContext}, {"OMP", OMPContext}, {"OMO", OMOContext},
}

func TestRootContextPublicRefusal(t *testing.T) {
	for _, api := range contextRootAPIs {
		t.Run(api.name, func(t *testing.T) {
			for _, state := range []string{"nil", "cancelled", "expired"} {
				t.Run(state, func(t *testing.T) {
					var ctx context.Context
					var want error
					switch state {
					case "cancelled":
						c, cancel := context.WithCancel(context.Background())
						cancel()
						ctx, want = c, context.Canceled
					case "expired":
						c, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
						defer cancel()
						ctx, want = c, context.DeadlineExceeded
					}
					got, err := api.fn(ctx, "/logical/home")
					if got != nil || err == nil || (want != nil && err != want) {
						t.Fatalf("roots=%v err=%v want=%v", got, err, want)
					}
					f := newRootFixture()
					got, err = f.d.roots(ctx, "/logical/home", api.name)
					if got != nil || err == nil || (want != nil && err != want) || len(f.calls) != 0 {
						t.Fatalf("private refusal roots=%v err=%v calls=%v", got, err, f.calls)
					}
				})
			}
		})
	}
}

func TestRootContextIncompleteDependencies(t *testing.T) {
	for _, missing := range []string{"getenv", "stat", "readDir", "clock", "cache", "entries"} {
		t.Run(missing, func(t *testing.T) {
			f := newRootFixture()
			switch missing {
			case "getenv":
				f.d.getenv = nil
			case "stat":
				f.d.stat = nil
			case "readDir":
				f.d.readDir = nil
			case "clock":
				f.d.now = nil
			case "cache":
				f.d.cache = nil
			case "entries":
				f.d.cache.entries = nil
			}
			for _, api := range contextRootAPIs {
				got, err := f.d.roots(context.Background(), "/logical/home", api.name)
				if got != nil || err == nil || len(f.calls) != 0 {
					t.Fatalf("%s roots=%v err=%v calls=%v", api.name, got, err, f.calls)
				}
			}
		})
	}
}

type rootCase struct{ name, list, single, base, leaf, db string }

var ordinaryRootCases = []rootCase{
	{"Claude", ClaudeListEnv, "CLAUDE_CONFIG_DIR", ".claude", "projects", ""},
	{"Qoder", QoderListEnv, "", ".qoder", "projects", ""},
	{"Codex", CodexListEnv, "CODEX_HOME", ".codex", "sessions", ""},
	{"Cursor", CursorListEnv, "", ".cursor/projects", "", ""},
	{"OpenCodeData", OpenCodeListEnv, "", ".local/share/opencode", "", ""},
	{"OpenCodeDBs", OpenCodeListEnv, "", ".local/share/opencode", "", "opencode.db"},
	{"HermesData", HermesListEnv, "HERMES_HOME", ".hermes", "", ""},
	{"HermesDBs", HermesListEnv, "HERMES_HOME", ".hermes", "", "state.db"},
	{"Pi", PiListEnv, "PI_CODING_AGENT_DIR", ".pi/agent", "sessions", ""},
	{"OMP", OMPListEnv, "PI_CODING_AGENT_DIR", ".omp/agent", "sessions", ""},
}

func prepareOrdinary(f *rootFixture, c rootCase, home string) {
	if c.name == "Pi" || c.name == "OMP" {
		f.statErrors[filepath.Join(home, filepath.Dir(c.base), "profiles")] = os.ErrNotExist
	}
}

func TestRootContextCompatibility(t *testing.T) {
	for _, c := range ordinaryRootCases {
		t.Run(c.name, func(t *testing.T) {
			for _, variant := range []string{"default", "configured", "invalid-home"} {
				t.Run(variant, func(t *testing.T) {
					f := newRootFixture()
					home := "/logical/home"
					if variant == "invalid-home" {
						home = ""
					}
					prepareOrdinary(f, c, home)
					base := filepath.Join(home, c.base)
					var want []string
					if variant != "default" {
						f.env[c.list] = strings.Join([]string{base, " /explicit ", "/explicit/./", "~/work", "~", "relative", "~other/x", " ", "/explicit"}, string(os.PathListSeparator))
						want = append(want, filepath.Join("/explicit", c.leaf, c.db))
						if home != "" {
							want = append(want, filepath.Join(home, "work", c.leaf, c.db), filepath.Join(home, c.leaf, c.db))
						}
						if c.single != "" {
							f.env[c.single] = "/single"
							want = append(want, filepath.Join("/single", c.leaf, c.db))
						}
					}
					if home != "" {
						want = append(want, filepath.Join(base, c.leaf, c.db))
					}
					got, err := f.d.roots(context.Background(), home, c.name)
					if err != nil || !slices.Equal(got, want) {
						t.Fatalf("got=%v err=%v want=%v", got, err, want)
					}
					f.check(t)
				})
			}
		})
	}
}

func TestRootContextXDG(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{"absolute", " /xdg "}, {"tilde", "~/data"}, {"relative", "relative"}, {"tilde-user", "~other/data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := test.value
			f := newRootFixture()
			f.env["XDG_DATA_HOME"] = value
			f.env[OpenCodeListEnv] = "/explicit"
			want := []string{"/explicit/opencode.db"}
			switch value {
			case " /xdg ":
				want = append(want, "/xdg/opencode/opencode.db")
			case "~/data":
				want = append(want, "/logical/home/data/opencode/opencode.db")
			}
			got, err := f.d.roots(context.Background(), "/logical/home", "OpenCodeDBs")
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("got=%v err=%v want=%v", got, err, want)
			}
			f.check(t)
		})
	}
}

func TestRootContextOMO(t *testing.T) {
	for _, state := range []string{"absent", "nonregular", "regular"} {
		t.Run(state, func(t *testing.T) {
			f := newRootFixture()
			home := "/logical/home"
			path := home + "/.omo/settings.json"
			switch state {
			case "absent":
				f.statErrors[path] = os.ErrNotExist
			case "nonregular":
				f.stats[path] = rootInfo{mode: os.ModeDir}
			case "regular":
				f.stats[path] = rootInfo{}
			}
			f.env[OMOListEnv] = home + "/.senpi/agent" + string(os.PathListSeparator) + "/relay" + string(os.PathListSeparator) + "/relay"
			f.env["OMO_CODING_AGENT_DIR"] = "/omo"
			f.env["SENPI_CODING_AGENT_DIR"] = "/senpi"
			f.env["PI_CODING_AGENT_DIR"] = "/pi"
			want := []string{"/relay/sessions", "/omo/sessions", "/senpi/sessions", "/pi/sessions", home + "/.omo/agent/sessions"}
			if state == "regular" {
				want = append(want, home+"/.omo/sessions")
			}
			want = append(want, home+"/.senpi/agent/sessions")
			got, err := f.d.roots(context.Background(), home, "OMO")
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("got=%v err=%v want=%v", got, err, want)
			}
			f.check(t)
		})
	}
}

func profileFixture(family string, dangling bool) (*rootFixture, string, []string) {
	f := newRootFixture()
	config := "/logical/home/.pi"
	if family == "OMP" {
		config = "/logical/home/.omp"
	}
	p := config + "/profiles"
	f.stats[p] = rootInfo{mode: os.ModeDir, stamp: 7, size: 12}
	f.reads[p] = []os.DirEntry{rootEntry{name: "a-link", mode: os.ModeSymlink}, rootEntry{name: "b-dir", mode: os.ModeDir}, rootEntry{name: "c-file"}}
	f.stats[p+"/a-link"] = rootInfo{mode: os.ModeDir}
	f.stats[p+"/b-dir"] = rootInfo{mode: os.ModeDir}
	f.stats[p+"/c-file"] = rootInfo{}
	if dangling {
		f.reads[p] = append(f.reads[p], rootEntry{name: "d-broken", mode: os.ModeSymlink})
		f.statErrors[p+"/d-broken"] = os.ErrNotExist
	}
	return f, config, []string{p + "/a-link/agent", p + "/b-dir/agent"}
}

func TestRootContextProfileCache(t *testing.T) {
	for _, family := range []string{"Pi", "OMP"} {
		t.Run(family, func(t *testing.T) {
			for _, state := range []string{"normal-60s", "dangling-5s"} {
				t.Run(state, func(t *testing.T) {
					f, config, want := profileFixture(family, state == "dangling-5s")
					ttl := profileCacheTTL
					if state == "dangling-5s" {
						ttl = profileDanglingCacheTTL
					}
					now := time.Unix(100, 0)
					f.times = []time.Time{now, now.Add(ttl - time.Nanosecond), now.Add(ttl), now.Add(ttl), now.Add(2 * ttl)}
					got, err := f.d.profiles(context.Background(), config)
					if err != nil || !slices.Equal(got, want) {
						t.Fatalf("initial=%v %v", got, err)
					}
					p := config + "/profiles"
					entry := f.d.cache.entries[p]
					if entry.signature != "7|12" || !entry.expires.Equal(now.Add(ttl)) {
						t.Fatalf("entry=%+v", entry)
					}
					got[0] = "caller mutation"
					start := len(f.calls)
					got, err = f.d.profiles(context.Background(), config)
					if err != nil || !slices.Equal(got, want) || !slices.Equal(f.calls[start:], []string{"stat:" + p, "clock"}) {
						t.Fatalf("hit=%v %v calls=%v", got, err, f.calls[start:])
					}
					got[0] = "hit mutation"
					if state == "dangling-5s" {
						delete(f.statErrors, p+"/d-broken")
						f.stats[p+"/d-broken"] = rootInfo{mode: os.ModeDir}
						want = append(want, p+"/d-broken/agent")
					}
					got, err = f.d.profiles(context.Background(), config)
					if err != nil || !slices.Equal(got, want) {
						t.Fatalf("expiry=%v %v", got, err)
					}
					f.stats[p] = rootInfo{mode: os.ModeDir, stamp: 8, size: 13}
					got, err = f.d.profiles(context.Background(), config)
					if err != nil || !slices.Equal(got, want) || f.d.cache.entries[p].signature != "8|13" {
						t.Fatalf("refresh=%v %v", got, err)
					}
					if len(f.times) != 0 {
						t.Errorf("unused clock values=%d", len(f.times))
					}
					f.check(t)
				})
			}
		})
	}
}

func TestRootContextDiscoveryFailures(t *testing.T) {
	for _, state := range []string{"stat-error", "not-directory", "read-error", "candidate-error"} {
		t.Run(state, func(t *testing.T) {
			f, config, _ := profileFixture("Pi", false)
			p := config + "/profiles"
			old := profileCacheEntry{signature: "old", dirs: []string{"/old"}, expires: time.Unix(50, 0)}
			f.d.cache.entries[p] = old
			switch state {
			case "stat-error":
				delete(f.stats, p)
				f.statErrors[p] = os.ErrPermission
			case "not-directory":
				f.stats[p] = rootInfo{}
			case "read-error":
				f.readErrors[p] = os.ErrPermission
			case "candidate-error":
				delete(f.stats, p+"/b-dir")
				f.statErrors[p+"/b-dir"] = os.ErrPermission
				f.times = []time.Time{time.Unix(100, 0)}
			}
			got, err := f.d.profiles(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "stat-error", "not-directory":
				if len(got) != 0 || len(f.d.cache.entries) != 0 {
					t.Fatalf("not invalidated: %v", got)
				}
			case "read-error":
				if len(got) != 0 || !reflect.DeepEqual(f.d.cache.entries[p], old) {
					t.Fatalf("read error altered cache: %v", got)
				}
			case "candidate-error":
				if !slices.Equal(got, []string{p + "/a-link/agent"}) {
					t.Fatalf("got=%v", got)
				}
			}
			f.check(t)
		})
	}
}

func TestRootContextBoundaryCancellation(t *testing.T) {
	for _, family := range []string{"Pi", "OMP"} {
		t.Run(family, func(t *testing.T) {
			for _, boundary := range []string{"before", "profiles-stat", "read-dir", "candidate-stat", "second-candidate-stat", "entry-name", "cache-hit-clock", "publication-clock", "invalidation-stat"} {
				t.Run(boundary, func(t *testing.T) {
					f, config, _ := profileFixture(family, false)
					p := config + "/profiles"
					old := profileCacheEntry{signature: "old", dirs: []string{"/preserved"}, expires: time.Unix(200, 0)}
					if boundary == "cache-hit-clock" {
						old.signature = "7|12"
					}
					f.d.cache.entries[p] = old
					f.times = []time.Time{time.Unix(100, 0)}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					target := ""
					switch boundary {
					case "before":
						cancel()
					case "profiles-stat", "invalidation-stat":
						target = "stat:" + p
					case "read-dir":
						target = "read:" + p
					case "candidate-stat":
						target = "stat:" + p + "/a-link"
					case "second-candidate-stat":
						target = "stat:" + p + "/b-dir"
					case "entry-name":
						f.reads[p][0] = rootEntry{name: "a-link", mode: os.ModeSymlink, onName: cancel}
					case "cache-hit-clock", "publication-clock":
						target = "clock"
					}
					if boundary == "invalidation-stat" {
						delete(f.stats, p)
						f.statErrors[p] = os.ErrPermission
					}
					capture, notices := -1, 0
					f.after = func(call string) {
						if call == target {
							if notices == 0 {
								capture = len(f.calls)
							}
							notices++
							cancel()
						}
					}
					got, err := f.d.roots(ctx, "/logical/home", family)
					if got != nil || err != context.Canceled {
						t.Fatalf("roots=%v err=%v", got, err)
					}
					if target != "" && (notices != 1 || len(f.calls) != capture) {
						t.Errorf("notices=%d capture=%d calls=%v", notices, capture, f.calls)
					}
					if boundary == "before" && len(f.calls) != 0 {
						t.Errorf("calls before work=%v", f.calls)
					}
					if boundary == "entry-name" && !slices.Equal(f.calls, []string{"stat:" + p, "read:" + p}) {
						t.Errorf("entry cancellation continued: %v", f.calls)
					}
					if !reflect.DeepEqual(f.d.cache.entries[p], old) {
						t.Error("cancelled operation poisoned independent old cache")
					}
					f.check(t)
				})
			}
		})
	}
}

func TestRootContextOMOCancellation(t *testing.T) {
	for _, boundary := range []string{"senpi-env", "pi-env", "legacy-stat", "list-env", "single-env"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRootFixture()
			path := "/logical/home/.omo/settings.json"
			f.stats[path] = rootInfo{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			target := map[string]string{"senpi-env": "env:SENPI_CODING_AGENT_DIR", "pi-env": "env:PI_CODING_AGENT_DIR", "legacy-stat": "stat:" + path, "list-env": "env:" + OMOListEnv, "single-env": "env:OMO_CODING_AGENT_DIR"}[boundary]
			capture, notices := -1, 0
			f.after = func(call string) {
				if call == target {
					if notices == 0 {
						capture = len(f.calls)
					}
					notices++
					cancel()
				}
			}
			got, err := f.d.roots(ctx, "/logical/home", "OMO")
			if got != nil || err != context.Canceled || notices != 1 || len(f.calls) != capture || len(f.d.cache.entries) != 0 {
				t.Fatalf("got=%v err=%v notices=%d calls=%v", got, err, notices, f.calls)
			}
			f.check(t)
		})
	}
}

// This context cancels a real WithCancel context at one chosen cooperative
// check. Sweeping every check covers configured/discovered normalization,
// default-last filtering and DB projection without adding production hooks.
type rootStepContext struct {
	context.Context
	cancel             context.CancelFunc
	at, checks, locked int
	cancelAfterCheck   bool
	effectCount        func() int
	atCancel           int
	cache              *profileCacheState
}

func (c *rootStepContext) Err() error {
	if c.cache.TryLock() {
		c.cache.Unlock()
	} else {
		c.locked++
	}
	c.checks++
	before := c.Context.Err()
	if c.at > 0 && c.checks == c.at {
		if c.effectCount != nil {
			c.atCancel = c.effectCount()
		}
		c.cancel()
	}
	if c.cancelAfterCheck {
		return before
	}
	return c.Context.Err()
}
func (c *rootStepContext) Done() <-chan struct{} {
	if c.cache.TryLock() {
		c.cache.Unlock()
	} else {
		c.locked++
	}
	return c.Context.Done()
}

func TestRootContextTraversalCancellation(t *testing.T) {
	cases := append([]rootCase(nil), ordinaryRootCases...)
	cases = append(cases, rootCase{"OMO", OMOListEnv, "OMO_CODING_AGENT_DIR", ".senpi/agent", "sessions", ""})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			makeCase := func(at int) (*rootFixture, *rootStepContext) {
				f := newRootFixture()
				prepareOrdinary(f, c, "/logical/home")
				if c.name == "OMO" {
					f.stats["/logical/home/.omo/settings.json"] = rootInfo{}
				}
				f.env[c.list] = strings.Join([]string{"/logical/home/" + c.base, "/a", "/b", "~/c", "relative", "/a"}, string(os.PathListSeparator))
				ctx, cancel := context.WithCancel(context.Background())
				return f, &rootStepContext{Context: ctx, cancel: cancel, at: at, cache: f.d.cache, effectCount: func() int { return len(f.calls) }}
			}
			f, baseline := makeCase(0)
			_, err := f.d.roots(baseline, "/logical/home", c.name)
			baseline.cancel()
			if err != nil {
				t.Fatal(err)
			}
			f.check(t)
			t.Run("every-check", func(t *testing.T) {
				for at := 1; at <= baseline.checks; at++ {
					f, ctx := makeCase(at)
					got, err := f.d.roots(ctx, "/logical/home", c.name)
					actual := ctx.Context.Err()
					ctx.cancel()
					if got != nil || err != context.Canceled || actual != context.Canceled || ctx.locked != 0 || len(f.calls) != ctx.atCancel {
						t.Fatalf("at=%d got=%v err=%v checks=%d locked=%d calls=%v captured=%d", at, got, err, ctx.checks, ctx.locked, f.calls, ctx.atCancel)
					}
					f.check(t)
				}
			})
		})
	}
}

func TestRootContextProfilePrecedence(t *testing.T) {
	for _, family := range []string{"Pi", "OMP"} {
		t.Run(family, func(t *testing.T) {
			f, config, dirs := profileFixture(family, false)
			list := PiListEnv
			if family == "OMP" {
				list = OMPListEnv
			}
			f.env[list] = strings.Join([]string{config + "/agent", "/relay", dirs[0], "/relay"}, string(os.PathListSeparator))
			f.env["PI_CODING_AGENT_DIR"] = "/single"
			f.times = []time.Time{time.Unix(100, 0)}
			want := []string{"/relay/sessions", dirs[0] + "/sessions", "/single/sessions", dirs[1] + "/sessions", config + "/agent/sessions"}
			got, err := f.d.roots(context.Background(), "/logical/home", family)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("got=%v err=%v want=%v", got, err, want)
			}
			// The same instance resolves changed configuration at call time.
			f.env[list] = "/changed"
			f.times = []time.Time{time.Unix(101, 0)}
			got, err = f.d.roots(context.Background(), "/logical/home", family)
			want = []string{"/changed/sessions", "/single/sessions", dirs[0] + "/sessions", dirs[1] + "/sessions", config + "/agent/sessions"}
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("dynamic got=%v err=%v want=%v", got, err, want)
			}
			f.check(t)
		})
	}
}

func TestRootContextPublicationChannelGuard(t *testing.T) {
	for _, family := range []string{"Pi", "OMP"} {
		t.Run(family, func(t *testing.T) {
			f, config, _ := profileFixture(family, false)
			f.times = []time.Time{time.Unix(100, 0)}
			base, cancel := context.WithCancel(context.Background())
			counter := &rootStepContext{Context: base, cancel: cancel, cache: f.d.cache}
			_, err := f.d.profiles(counter, config)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			f.check(t)
			f, config, _ = profileFixture(family, false)
			f.times = []time.Time{time.Unix(100, 0)}
			old := profileCacheEntry{signature: "old", dirs: []string{"/preserved"}, expires: time.Unix(200, 0)}
			f.d.cache.entries[config+"/profiles"] = old
			base, cancel = context.WithCancel(context.Background())
			defer cancel()
			// Last check before publication observes nil, then cancellation is
			// requested before the cache lock. The obtained Done channel must
			// prevent publication without calling context methods under lock.
			ctx := &rootStepContext{Context: base, cancel: cancel, cache: f.d.cache, at: counter.checks - 1, cancelAfterCheck: true}
			got, err := f.d.profiles(ctx, config)
			if got != nil || err != context.Canceled || ctx.locked != 0 || !reflect.DeepEqual(f.d.cache.entries[config+"/profiles"], old) {
				t.Fatalf("got=%v err=%v locked=%d cache=%v", got, err, ctx.locked, f.d.cache.entries)
			}
			f.check(t)
		})
	}
}

func TestRootContextDependencyDenial(t *testing.T) {
	for _, effect := range []string{"environment", "stat", "readDir", "clock"} {
		t.Run(effect, func(t *testing.T) {
			f := newRootFixture()
			switch effect {
			case "environment":
				delete(f.env, ClaudeListEnv)
				_, _ = f.d.roots(context.Background(), "/logical/home", "Claude")
			case "stat":
				_, _ = f.d.profiles(context.Background(), "/logical/home/.pi")
			case "readDir":
				f.stats["/logical/home/.pi/profiles"] = rootInfo{mode: os.ModeDir}
				_, _ = f.d.profiles(context.Background(), "/logical/home/.pi")
			case "clock":
				f.stats["/logical/home/.pi/profiles"] = rootInfo{mode: os.ModeDir}
				f.reads["/logical/home/.pi/profiles"] = nil
				_, _ = f.d.profiles(context.Background(), "/logical/home/.pi")
			}
			if f.denied != 1 || f.locked != 0 {
				t.Fatalf("denied=%d locked=%d calls=%v", f.denied, f.locked, f.calls)
			}
		})
	}
}

func TestRootContextHeldSynchronousCall(t *testing.T) {
	f, config, _ := profileFixture("Pi", false)
	p := config + "/profiles"
	ctx, cancel := context.WithCancel(context.Background())
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	retired := make(chan struct{})
	stopRetirement := context.AfterFunc(ctx, func() { close(retired) })
	var releaseOnce sync.Once
	var got []string
	var err error
	notices := 0
	// Register completion and repeat-safe cleanup before the only launch.
	cleanup := func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		<-done
		if !stopRetirement() {
			<-retired
		}
	}
	t.Cleanup(cleanup)
	f.after = func(call string) {
		if call == "read:"+p {
			notices++
			if notices == 1 {
				close(entered)
			}
			<-release
		}
	}
	go func() { defer close(done); got, err = f.d.roots(ctx, "/logical/home", "Pi") }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("caller returned before entering held dependency")
	}
	cancel()
	<-retired // actual cancellation callback entered while the read remains held
	// The dependency has positively entered and remains held for this bounded
	// interval. Observe the actual caller, not a newly launched proxy.
	timer := time.NewTimer(20 * time.Millisecond)
	select {
	case <-done:
		t.Error("actual caller detached from held synchronous dependency")
	case <-timer.C:
	}
	timer.Stop()
	cleanup() // cancel -> release all -> join actual caller -> inspect
	if got != nil || err != context.Canceled || notices != 1 || len(f.d.cache.entries) != 0 {
		t.Errorf("got=%v err=%v notices=%d cache=%v", got, err, notices, f.d.cache.entries)
	}
	if !slices.Equal(f.calls, []string{"stat:" + p, "read:" + p}) {
		t.Errorf("calls after cancellation=%v", f.calls)
	}
	f.check(t)
}
