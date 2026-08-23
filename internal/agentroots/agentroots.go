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
// agent's own single-directory variable, then the home default. The home default
// is always appended, so configuring the list adds profiles instead of replacing
// the default profile.
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

// Pi reports the session roots for the Pi coding agent.
func Pi(home string) []string {
	return resolve(PiListEnv, "PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"), "sessions")
}

// OMP reports the session roots for Oh My Pi. Oh My Pi is a Pi fork and reads
// the same PI_CODING_AGENT_DIR override for its default profile; named profiles
// ignore that variable and live under <config root>/profiles/<name>/agent,
// which is what HERDR_OMP_CONFIG_DIRS is for.
func OMP(home string) []string {
	return resolve(OMPListEnv, "PI_CODING_AGENT_DIR", filepath.Join(home, ".omp", "agent"), "sessions")
}

// resolve builds the ordered, de-duplicated root list for one agent. singleEnv
// may be empty for agents without a config directory variable. leaf may be
// empty to report the config directories themselves.
func resolve(listEnv, singleEnv, homeBase, leaf string) []string {
	seen := make(map[string]bool, 4)
	roots := make([]string, 0, 4)
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
	add(homeBase)
	return roots
}
