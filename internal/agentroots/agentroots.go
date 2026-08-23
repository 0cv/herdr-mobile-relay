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
// Both the conversation reader and the session-title resolver resolve through
// this package, which is what keeps a pane's title and its transcript from
// being looked up in different trees.
package agentroots

import (
	"os"
	"path/filepath"
	"strings"
)

// Relay-side overrides. Each holds a platform-separated list (":" on Unix, ";"
// on Windows, as in PATH) of *config directories* - the same values that would
// go in CLAUDE_CONFIG_DIR and friends - not pre-joined transcript roots. The
// service wrapper exports every key of relay.env into the relay process, so
// these belong in that file.
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
	return resolve(ClaudeListEnv, "CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"), "projects")
}

// Qoder reports the transcript roots for Qoder CLI, which has no config
// directory variable of its own.
func Qoder(home string) []string {
	return resolve(QoderListEnv, "", filepath.Join(home, ".qoder"), "projects")
}

// Codex reports the rollout roots for OpenAI Codex.
func Codex(home string) []string {
	return resolve(CodexListEnv, "CODEX_HOME", filepath.Join(home, ".codex"), "sessions")
}

// CodexHomes reports the Codex config directories themselves, for consumers
// that read a file stored beside the sessions tree (session_index.jsonl).
func CodexHomes(home string) []string {
	return resolve(CodexListEnv, "CODEX_HOME", filepath.Join(home, ".codex"), "")
}

// Pi reports the session roots for the Pi coding agent, including the agent
// directory of every named profile found under its config root.
func Pi(home string) []string {
	configRoot := filepath.Join(home, ".pi")
	return resolve(PiListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "sessions",
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
	return resolve(OMPListEnv, "PI_CODING_AGENT_DIR", filepath.Join(configRoot, "agent"), "sessions",
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

// resolve builds the ordered, de-duplicated root list for one agent. singleEnv
// may be empty for agents without a config directory variable. leaf may be
// empty to report the config directories themselves. discovered bases are
// appended after the environment ones and before the home default, so
// configuration always wins over discovery and the home default stays last.
func resolve(listEnv, singleEnv, homeBase, leaf string, discovered ...string) []string {
	seen := make(map[string]bool, 4+len(discovered))
	roots := make([]string, 0, 4+len(discovered))
	add := func(base string) {
		base = strings.TrimSpace(base)
		if base == "" {
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
