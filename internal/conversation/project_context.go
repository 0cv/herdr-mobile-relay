package conversation

import (
	"path/filepath"
	"strings"
)

// ProjectContext identifies the directories that may own an agent's
// conversation. CWD is the pane's reported directory; ForegroundCWD is a more
// specific live hint supplied by Herdr for Claude Code.
type ProjectContext struct {
	CWD           string
	ForegroundCWD string
}

// NormalizeProjectContext applies the shared project-selection policy. The
// pane cwd keeps its existing semantics, while the foreground hint is accepted
// only when it is an absolute Claude directory and is not redundant.
// Non-Claude providers deliberately remain pane-only in this fix.
func NormalizeProjectContext(agent string, project ProjectContext) ProjectContext {
	// Keep the pane cwd byte-for-byte intact: its existing lookup semantics
	// include any nonempty suffix, so only the new hint is trimmed.
	project.ForegroundCWD = strings.TrimSpace(project.ForegroundCWD)
	if !isClaudeProvider(agent) || project.ForegroundCWD == "" || !filepath.IsAbs(project.ForegroundCWD) ||
		project.ForegroundCWD == project.CWD {
		project.ForegroundCWD = ""
	}
	return project
}

// normalizeBrowseProjectContext preserves Claude's pane-cwd lookup semantics
// for both source selection and cursor scope IDs. Other providers retain the
// browser's historical whitespace trimming and never accept a foreground hint.
func normalizeBrowseProjectContext(agent, cwd, foregroundCWD string) ProjectContext {
	if !isClaudeProvider(agent) {
		cwd = strings.TrimSpace(cwd)
	}
	return NormalizeProjectContext(agent, ProjectContext{CWD: cwd, ForegroundCWD: foregroundCWD})
}
