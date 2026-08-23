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
	"os"
	"path/filepath"
	"strings"
)

// Relay-side overrides. Each holds a colon-separated list (the platform
// path-list separator, as in PATH) of directories - not pre-joined transcript
// roots. For ClaudeListEnv, QoderListEnv and CodexListEnv each entry is a
// *config directory* - the same values that would go in CLAUDE_CONFIG_DIR and
// friends. For PiListEnv and OMPListEnv each entry is instead an *agent*
// directory - the same value PI_CODING_AGENT_DIR takes - because resolve()
// joins the leaf straight onto it, so a config-root directory there would
// scan one level too high. The service wrapper exports every key of
// relay.env into the relay process, so these belong in that file.
const (
	ClaudeListEnv = "HERDR_CLAUDE_CONFIG_DIRS"
	QoderListEnv  = "HERDR_QODER_CONFIG_DIRS"
	CodexListEnv  = "HERDR_CODEX_CONFIG_DIRS"
	PiListEnv     = "HERDR_PI_CONFIG_DIRS"
	OMPListEnv    = "HERDR_OMP_CONFIG_DIRS"
)

// Claude reports the transcript roots for Claude Code, honouring
// CLAUDE_CONFIG_DIR exactly as it did when a single root was resolved.
func Claude(home string) []string {
	return resolve(home, ClaudeListEnv, "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"), "projects")
}

// Qoder reports the transcript roots for Qoder CLI, which has no config
// directory variable of its own.
func Qoder(home string) []string {
	return resolve(home, QoderListEnv, "", filepath.Join(home, ".qoder"), "projects")
}

// Codex reports the rollout roots for OpenAI Codex.
func Codex(home string) []string {
	return resolve(home, CodexListEnv, "CODEX_HOME", filepath.Join(home, ".codex"), "sessions")
}

// CodexHomes reports the Codex config directories themselves, for consumers
// that read a file stored beside the sessions tree (session_index.jsonl).
func CodexHomes(home string) []string {
	return resolve(home, CodexListEnv, "CODEX_HOME", filepath.Join(home, ".codex"), "")
}

// Pi reports the session roots for the Pi coding agent, including the agent
// directory of every named profile found under its config root.
func Pi(home string) []string {
	configRoot := filepath.Join(home, ".pi")
	return resolve(home, PiListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "sessions",
		profileAgentDirs(configRoot)...)
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
	configRoot := filepath.Join(home, ".omp")
	return resolve(home, OMPListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "sessions",
		profileAgentDirs(configRoot)...)
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
	// A relative config root would make this scan directories under the relay's
	// working directory, which is never what the caller means. This happens only
	// when os.UserHomeDir() failed and home is empty.
	if !filepath.IsAbs(configRoot) {
		return nil
	}
	profiles := filepath.Join(configRoot, "profiles")
	entries, err := os.ReadDir(profiles)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		candidate := filepath.Join(profiles, entry.Name())
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		dirs = append(dirs, filepath.Join(candidate, "agent"))
	}
	return dirs
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
	for _, base := range filepath.SplitList(os.Getenv(listEnv)) {
		add(base)
	}
	if singleEnv != "" {
		add(os.Getenv(singleEnv))
	}
	for _, base := range discovered {
		add(base)
	}
	add(homeBase)
	return roots
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
