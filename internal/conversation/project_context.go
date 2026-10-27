package conversation

import (
	"path/filepath"
	"strings"
)

// ProjectContext identifies the directories that may own an agent's
// conversation. CWD is the pane's reported directory; ForegroundCWD is a more
// specific live hint supplied by Herdr for Claude Code and Kimi Code.
type ProjectContext struct {
	CWD           string
	ForegroundCWD string
}

// NormalizeProjectContext applies the shared project-selection policy. The
// pane cwd keeps its existing semantics, while the foreground hint is accepted
// only for Claude or Kimi, when it is absolute and is not redundant.
func NormalizeProjectContext(agent string, project ProjectContext) ProjectContext {
	// Keep the pane cwd byte-for-byte intact: its existing lookup semantics
	// include any nonempty suffix. Kimi also preserves its foreground spelling.
	if !IsKimi(agent) {
		project.ForegroundCWD = strings.TrimSpace(project.ForegroundCWD)
	}
	if (!isClaudeProvider(agent) && !IsKimi(agent)) || project.ForegroundCWD == "" || !filepath.IsAbs(project.ForegroundCWD) ||
		project.ForegroundCWD == project.CWD {
		project.ForegroundCWD = ""
	}
	return project
}

// normalizeBrowseProjectContext preserves Claude and Kimi directory spelling
// for source selection and cursor scope IDs. Other providers trim whitespace.
func normalizeBrowseProjectContext(agent, cwd, foregroundCWD string) ProjectContext {
	if !isClaudeProvider(agent) && !IsKimi(agent) {
		cwd = strings.TrimSpace(cwd)
	}
	return NormalizeProjectContext(agent, ProjectContext{CWD: cwd, ForegroundCWD: foregroundCWD})
}
