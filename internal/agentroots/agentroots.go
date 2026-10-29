// Package agentroots resolves the directories the relay searches for a coding
// agent's transcript and session files.
//
// The relay runs as a launchd/systemd user service, so it does not inherit the
// shell environment herdr uses when it spawns an agent pane. When an agent is
// configured with a non-default config directory - one profile per herdr setup -
// that per-pane value is invisible to the relay by construction.
//
// Every agent therefore resolves to a *list* of roots rather than a single one.
// Order is: the relay's own platform path list (HERDR_<AGENT>_CONFIG_DIRS), the
// agent's own single-directory variable, then any profile directories discovered
// on disk (Pi and Oh My Pi only), then the home default. The home default is
// always appended, so configuring the list adds profiles instead of replacing
// the default profile, and configuration always outranks discovery.
//
// Every configured entry is normalised before use: a leading "~" or "~/" is
// expanded against the caller's home directory, and whatever remains must then
// be absolute or the entry is dropped. This matters specifically because the
// relay runs under launchd/systemd, never a shell: a silently relative entry
// resolves against the service's working directory instead of erroring, which
// produces the exact missing-history symptom this package exists to cure and
// is indistinguishable from a plain wrong path once it happens. It also
// matters because bash does not expand "~" inside the quotes the README
// advises for a value containing spaces, so the tilde form has to be honoured
// here or the README's own worked example never actually works.
//
// Both the conversation reader and the session-title resolver resolve through
// this package, which is what keeps a pane's title and its transcript from
// being looked up in different trees.
package agentroots

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Relay-side overrides. Each holds a colon-separated list (the platform
// path-list separator, as in PATH) of directories - not pre-joined transcript
// roots. For ClaudeListEnv, QoderListEnv and CodexListEnv each entry is a
// *config directory* - the same values that would go in CLAUDE_CONFIG_DIR and
// friends. For PiListEnv and OMPListEnv each entry is instead an *agent*
// directory - the same value PI_CODING_AGENT_DIR takes - because resolve()
// joins the leaf straight onto it, so a config-root directory there would
// scan one level too high. CursorListEnv is the exception to the "not
// pre-joined" rule: Cursor has no config directory variable, and each entry is
// already a projects directory (the same shape as ~/.cursor/projects), joined
// with no leaf. The service wrapper exports every key of
// relay.env into the relay process, so these belong in that file.
const (
	ClaudeListEnv   = "HERDR_CLAUDE_CONFIG_DIRS"
	QoderListEnv    = "HERDR_QODER_CONFIG_DIRS"
	CodexListEnv    = "HERDR_CODEX_CONFIG_DIRS"
	PiListEnv       = "HERDR_PI_CONFIG_DIRS"
	OMPListEnv      = "HERDR_OMP_CONFIG_DIRS"
	OpenCodeListEnv = "HERDR_OPENCODE_DATA_DIRS"
	OMOListEnv      = "HERDR_OMO_CONFIG_DIRS"
	HermesListEnv   = "HERDR_HERMES_DATA_DIRS"
	CursorListEnv   = "HERDR_CURSOR_PROJECTS_DIRS"
)

// Claude reports the transcript roots for Claude Code, honouring
// CLAUDE_CONFIG_DIR exactly as it did when a single root was resolved.
func Claude(home string) []string {
	roots, _ := ClaudeContext(context.Background(), home)
	return roots
}

// Qoder reports the transcript roots for Qoder CLI, which has no config
// directory variable of its own.
func Qoder(home string) []string {
	roots, _ := QoderContext(context.Background(), home)
	return roots
}

// Codex reports the rollout roots for OpenAI Codex.
func Codex(home string) []string {
	roots, _ := CodexContext(context.Background(), home)
	return roots
}

// CodexHomes reports the Codex config directories themselves, for consumers
// that read a file stored beside the sessions tree (session_index.jsonl).
func CodexHomes(home string) []string {
	return resolve(home, CodexListEnv, "CODEX_HOME", filepath.Join(home, ".codex"), "")
}

// Cursor reports the project roots that hold Cursor Agent transcripts under
// <root>/<slug>/agent-transcripts/<uuid>/<uuid>.jsonl. List entries are the
// projects directories themselves (the same shape as ~/.cursor/projects).
func Cursor(home string) []string {
	roots, _ := CursorContext(context.Background(), home)
	return roots
}

// OpenCodeData reports the only directories in which the relay will accept an
// OpenCode database. Entries name data directories, not database files.
func OpenCodeData(home string) []string {
	roots, _ := OpenCodeDataContext(context.Background(), home)
	return roots
}

// OpenCodeDBs reports fixed database filenames under the configured OpenCode
// data directories. A caller must still re-check containment and regular-file
// status immediately before opening one.
func OpenCodeDBs(home string) []string {
	roots, _ := OpenCodeDBsContext(context.Background(), home)
	return roots
}

// HermesData reports the directories containing Hermes Agent state databases.
// Hermes stores its canonical session history in state.db under HERMES_HOME,
// defaulting to ~/.hermes.
func HermesData(home string) []string {
	roots, _ := HermesDataContext(context.Background(), home)
	return roots
}

// HermesDBs reports the fixed state database filename under each Hermes data
// directory. Callers must still re-check containment and regular-file status
// immediately before opening one.
func HermesDBs(home string) []string {
	roots, _ := HermesDBsContext(context.Background(), home)
	return roots
}

// OMO reports every supported Pi-compatible transcript root for Oh My
// OpenAgent. The relay cannot see a pane's environment, so it scans the
// branded, standalone Senpi, and pre-unification layouts in addition to any
// relay-side or process environment override.
func OMO(home string) []string {
	roots, _ := OMOContext(context.Background(), home)
	return roots
}

// Pi reports the session roots for the Pi coding agent, including the agent
// directory of every named profile found under its config root.
func Pi(home string) []string {
	roots, _ := PiContext(context.Background(), home)
	return roots
}

// OMP reports the session roots for Oh My Pi, including the agent directory of
// every named profile found under its config root.
//
// Oh My Pi is a Pi fork and reads the same PI_CODING_AGENT_DIR override, but
// only while no profile is active: verified against the omp 18.0.3 bundle, where
// `agentDirOverride` is passed as `t ? undefined : e.agentDirOverride` and `t` is
// the resolved profile. There is no OMP_AGENT_DIR, OMP_CODING_AGENT_DIR or
// OMP_CONFIG_DIR - none of those strings occur in the binary at all.
//
// Note the asymmetry, because it is easy to misread the above as "a profile has
// nothing to do with this variable": omp *ignores* PI_CODING_AGENT_DIR as an
// input under a profile, but it *exports* the resolved agent directory back into
// the environment of what it spawns, so a pane running a profile does carry a
// PI_CODING_AGENT_DIR pointing at that profile. That is of no use here - the
// relay cannot read a pane's environment, which is the whole reason this package
// exists - but it is why discovery, not the variable, is what finds profiles.
func OMP(home string) []string {
	roots, _ := OMPContext(context.Background(), home)
	return roots
}

// OMPSkillDirs reports the directories holding Oh My Pi's own (native) skills,
// one per agent directory resolved for OMP.
func OMPSkillDirs(home string) []string {
	configRoot := filepath.Join(home, ".omp")
	return resolve(home, OMPListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "skills",
		profileAgentDirs(configRoot)...)
}

// OMPManagedSkillDirs reports the directories holding Oh My Pi's managed skills,
// the ones the agent mints for itself.
func OMPManagedSkillDirs(home string) []string {
	configRoot := filepath.Join(home, ".omp")
	return resolve(home, OMPListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "managed-skills",
		profileAgentDirs(configRoot)...)
}

// OMPConfigDirs reports the Oh My Pi agent directories themselves, where
// config.yml lives. A named profile keeps its own config file, so this is the
// only way to reach the settings that actually apply to a profile's pane.
func OMPConfigDirs(home string) []string {
	configRoot := filepath.Join(home, ".omp")
	return resolve(home, OMPListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "",
		profileAgentDirs(configRoot)...)
}

// PiSkillDirs reports the directories holding Pi's own skills.
func PiSkillDirs(home string) []string {
	configRoot := filepath.Join(home, ".pi")
	return resolve(home, PiListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "skills",
		profileAgentDirs(configRoot)...)
}

// PiConfigDirs reports the Pi agent directories themselves, where settings.json
// lives.
func PiConfigDirs(home string) []string {
	configRoot := filepath.Join(home, ".pi")
	return resolve(home, PiListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "",
		profileAgentDirs(configRoot)...)
}

// AgentDirForSession returns the Pi/OMP agent directory that contains an
// absolute session path. It lets pane-scoped skill discovery select the active
// profile instead of merging every profile into one catalog.
func AgentDirForSession(home, agent, sessionPath string) string {
	var roots []string
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(agent)))
	switch normalized {
	case "pi", "picodingagent":
		roots = Pi(home)
	case "omp", "ohmypi":
		roots = OMP(home)
	default:
		return ""
	}
	for _, root := range roots {
		if containedPath(sessionPath, root) {
			return filepath.Dir(root)
		}
	}
	return ""
}

func containedPath(path, root string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(realRoot, realPath)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

const (
	profileCacheTTL         = 60 * time.Second
	profileDanglingCacheTTL = 5 * time.Second
)

type profileCacheEntry struct {
	signature string
	dirs      []string
	expires   time.Time
}

type profileCacheState struct {
	sync.Mutex
	entries map[string]profileCacheEntry
}

var profileCache = profileCacheState{entries: make(map[string]profileCacheEntry)}

// Dependencies are complete per operation; fixtures never borrow production
// callbacks or cache state. Cached slices are immutable after publication.
type rootDependencies struct {
	getenv  func(string) string
	stat    func(string) (os.FileInfo, error)
	readDir func(string) ([]os.DirEntry, error)
	now     func() time.Time
	cache   *profileCacheState
}

func productionRoots() rootDependencies {
	return rootDependencies{os.Getenv, os.Stat, os.ReadDir, time.Now, &profileCache}
}

func rootContextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("agentroots: nil context")
	}
	return ctx.Err()
}

func (d rootDependencies) validate(ctx context.Context) error {
	if err := rootContextError(ctx); err != nil {
		return err
	}
	if d.getenv == nil || d.stat == nil || d.readDir == nil || d.now == nil || d.cache == nil {
		return errors.New("agentroots: incomplete dependencies")
	}
	d.cache.Lock()
	complete := d.cache.entries != nil
	d.cache.Unlock()
	if !complete {
		return errors.New("agentroots: missing cache state")
	}
	return nil
}

// Only an already obtained channel is inspected under the cache mutex.
func rootCancelled(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// profileAgentDirs reports the agent directory of every named profile under a
// config root. Pi and Oh My Pi place one at <config root>/profiles/<name>/agent
// - verified against the omp bundle, which builds exactly
// `join(configRoot, "profiles", name, "agent")` - and a profile makes the agent
// ignore PI_CODING_AGENT_DIR, so a profile's sessions are reachable no other
// way. Discovering them is what lets the common one-profile-per-herdr-setup
// layout work with no configuration at all.
//
// Only the home default's config root is expanded. An explicitly configured
// entry is itself an agent directory, so treating its parent as a config root
// would be wrong. A relocated config root - omp resolves that through
// PI_CONFIG_DIR and XDG probing - is what the HERDR_*_CONFIG_DIRS lists are
// for; mirroring that resolution here would couple the relay to omp's
// internals and drift out of step with them.
//
// os.ReadDir sorts its result, so the discovered order is deterministic.
//
// Entries are classified with os.Stat, not DirEntry.IsDir. DirEntry reports the
// directory entry's own type bits, which are ModeSymlink for a symlink, so
// IsDir() is false for a symlink to a directory and a profile reached through
// one would be silently skipped. omp joins the path and follows it, so the
// relay has to as well; symlinked profile directories are a normal way to keep
// agent configuration on another volume or in a dotfiles repo.
func profileAgentDirs(configRoot string) []string {
	dirs, _ := productionRoots().profiles(context.Background(), configRoot)
	return dirs
}

func (d rootDependencies) profiles(ctx context.Context, configRoot string) ([]string, error) {
	if err := d.validate(ctx); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(configRoot) {
		return nil, rootContextError(ctx)
	}
	profiles := filepath.Join(configRoot, "profiles")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, statErr := d.stat(profiles)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	done := ctx.Done()
	if statErr != nil || !info.IsDir() {
		d.cache.Lock()
		cancelled := rootCancelled(done)
		if !cancelled {
			delete(d.cache.entries, profiles)
		}
		d.cache.Unlock()
		return nil, ctx.Err()
	}
	signature := strconv.FormatInt(info.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(info.Size(), 10)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.cache.Lock()
	cached, ok := d.cache.entries[profiles]
	d.cache.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ok && cached.signature == signature {
		now := d.now()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if now.Before(cached.expires) {
			var dirs []string
			for _, dir := range cached.dirs {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				dirs = append(dirs, dir)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return dirs, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, readErr := d.readDir(profiles)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, nil
	}
	dirs := make([]string, 0, len(entries))
	dangling := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidate := filepath.Join(profiles, entry.Name())
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entryInfo, statErr := d.stat(candidate)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if statErr != nil {
			if entry.Type()&os.ModeSymlink != 0 {
				dangling = true
			}
			continue
		}
		if !entryInfo.IsDir() {
			continue
		}
		dirs = append(dirs, filepath.Join(candidate, "agent"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ttl := profileCacheTTL
	if dangling {
		ttl = profileDanglingCacheTTL
	}
	var owned []string
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		owned = append(owned, dir)
	}
	publication := profileCacheEntry{signature: signature, dirs: owned, expires: d.now().Add(ttl)}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.cache.Lock()
	cancelled := rootCancelled(done)
	if !cancelled {
		d.cache.entries[profiles] = publication
	}
	d.cache.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return dirs, nil
}

// resolve builds the ordered, de-duplicated root list for one agent. home is
// threaded through only so add() can expand a leading "~" in a *configured*
// entry - homeBase is already an absolute path the caller built from home
// directly, so home is not otherwise used here. singleEnv may be empty for
// agents without a config directory variable. leaf may be empty to report the
// config directories themselves. discovered bases are appended after the
// environment ones and before the home default, so configuration always wins
// over discovery and the home default stays last.
//
// Any base still not absolute after that expansion is dropped instead of
// joined - including homeBase, which is relative whenever the caller's
// os.UserHomeDir() failed - because relay.env is read by a service and never
// expanded by a shell, so a "~" or "." entry would otherwise silently become a
// scan root under the service's working directory.
func resolve(home, listEnv, singleEnv, homeBase, leaf string, discovered ...string) []string {
	roots, _ := productionRoots().resolveContext(context.Background(), home, listEnv, singleEnv, homeBase, leaf, discovered...)
	return roots
}

func (d rootDependencies) resolveContext(ctx context.Context, home, listEnv, singleEnv, homeBase, leaf string, discovered ...string) ([]string, error) {
	if err := d.validate(ctx); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, 4+len(discovered))
	roots := make([]string, 0, 4+len(discovered))
	add := func(base string) {
		base = strings.TrimSpace(base)
		if base == "" {
			return
		}
		// The README's only worked example configures a path as
		// "~/agents/claude-work", and the same paragraph advises quoting
		// values that contain spaces - but bash does not expand "~" inside
		// quotes, so the documented syntax would otherwise silently degrade
		// to a relative path. Expand it ourselves, before the absolute-path
		// check below, so a home-relative entry is not rejected as relative.
		base = expandTilde(base, home)
		// Anything still relative here would join against the relay's
		// working directory instead of the caller's intended root, which
		// produces the exact missing-history symptom this package exists to
		// cure and looks indistinguishable from a plain wrong path once it
		// happens - the process has no shell to have expanded or validated
		// the value first. profileAgentDirs already refuses a relative
		// config root for the same reason; this closes the matching half of
		// that hazard so the package does not carry two contracts for one
		// thing.
		if !filepath.IsAbs(base) {
			return
		}
		root := filepath.Join(base, leaf)
		if seen[root] {
			return
		}
		seen[root] = true
		roots = append(roots, root)
	}
	for _, base := range filepath.SplitList(d.getenv(listEnv)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		add(base)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if singleEnv != "" {
		base := d.getenv(singleEnv)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		add(base)
	}
	for _, base := range discovered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		add(base)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	add(homeBase)

	// The home default is a fallback, even when it was also named explicitly.
	// Remove its earlier occurrence and append it once at the end.
	homeRoot := filepath.Clean(filepath.Join(expandTilde(strings.TrimSpace(homeBase), home), leaf))
	if filepath.IsAbs(homeRoot) {
		filtered := roots[:0]
		for _, root := range roots {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if root != homeRoot {
				filtered = append(filtered, root)
			}
		}
		roots = append(filtered, homeRoot)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return roots, nil
}

// Context counterparts use cancellation only, not Reader lifetime authority.
func ClaudeContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "Claude")
}

func QoderContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "Qoder")
}

func CodexContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "Codex")
}

func CursorContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "Cursor")
}

func OpenCodeDataContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "OpenCodeData")
}

func OpenCodeDBsContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "OpenCodeDBs")
}

func HermesDataContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "HermesData")
}

func HermesDBsContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "HermesDBs")
}

func PiContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "Pi")
}

func OMPContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "OMP")
}

func OMOContext(ctx context.Context, home string) ([]string, error) {
	return productionRoots().roots(ctx, home, "OMO")
}

func (d rootDependencies) roots(ctx context.Context, home, family string) ([]string, error) {
	if err := d.validate(ctx); err != nil {
		return nil, err
	}
	var list, single, base, leaf, database string
	var discovered []string
	switch family {
	case "Claude":
		list, single, base, leaf = ClaudeListEnv, "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"), "projects"
	case "Qoder":
		list, base, leaf = QoderListEnv, filepath.Join(home, ".qoder"), "projects"
	case "Codex":
		list, single, base, leaf = CodexListEnv, "CODEX_HOME", filepath.Join(home, ".codex"), "sessions"
	case "Cursor":
		list, base = CursorListEnv, filepath.Join(home, ".cursor", "projects")
	case "OpenCodeData", "OpenCodeDBs":
		xdg := strings.TrimSpace(d.getenv("XDG_DATA_HOME"))
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if xdg == "" {
			xdg = filepath.Join(home, ".local", "share")
		}
		list, base = OpenCodeListEnv, filepath.Join(xdg, "opencode")
		if family == "OpenCodeDBs" {
			database = "opencode.db"
		}
	case "HermesData", "HermesDBs":
		list, single, base = HermesListEnv, "HERMES_HOME", filepath.Join(home, ".hermes")
		if family == "HermesDBs" {
			database = "state.db"
		}
	case "Pi", "OMP":
		config := filepath.Join(home, ".pi")
		list = PiListEnv
		if family == "OMP" {
			config, list = filepath.Join(home, ".omp"), OMPListEnv
		}
		var err error
		discovered, err = d.profiles(ctx, config)
		if err != nil {
			return nil, err
		}
		single, base, leaf = "PI_CODING_AGENT_DIR", filepath.Join(config, "agent"), "sessions"
	case "OMO":
		for _, key := range []string{"SENPI_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			discovered = append(discovered, d.getenv(key))
		}
		discovered = append(discovered, filepath.Join(home, ".omo", "agent"))
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		legacy := filepath.Join(home, ".omo")
		info, statErr := d.stat(filepath.Join(legacy, "settings.json"))
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if statErr == nil && info.Mode().IsRegular() {
			discovered = append(discovered, legacy)
		}
		list, single, base, leaf = OMOListEnv, "OMO_CODING_AGENT_DIR", filepath.Join(home, ".senpi", "agent"), "sessions"
	default:
		return nil, errors.New("agentroots: unknown family")
	}
	roots, err := d.resolveContext(ctx, home, list, single, base, leaf, discovered...)
	if err != nil {
		return nil, err
	}
	if database != "" {
		for i, root := range roots {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			roots[i] = filepath.Join(root, database)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return roots, nil
}

// expandTilde expands a leading "~" or "~/" against home, the same shorthand a
// shell would expand before a program ever saw the value. It exists only
// because this package reads its input straight from the environment of a
// launchd/systemd service, which is never a shell, so nothing upstream has
// already done this expansion; without it, a value copied verbatim from the
// README's "~/agents/claude-work" example would resolve as a relative path
// and be dropped by the absolute-path guard in add(). A bare "~" alone means
// home itself. "~user" forms, which name another account's home directory,
// are deliberately left untouched rather than guessed at - they fall through
// unchanged and are then rejected as relative by the same guard, like any
// other relative entry.
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
