package slashcmd

import "testing"

// isolateAgentEnv neutralises every environment variable that agentroots reads
// from the process environment, so a developer's own agent configuration cannot
// change what a test discovers. resolve trims and discards empty values, so an
// empty variable is equivalent to an unset one.
func isolateAgentEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PI_CODING_AGENT_DIR",
		"HERDR_OMP_CONFIG_DIRS",
		"HERDR_PI_CONFIG_DIRS",
		"HERDR_CLAUDE_CONFIG_DIRS",
		"HERDR_QODER_CONFIG_DIRS",
		"HERDR_CODEX_CONFIG_DIRS",
		"CLAUDE_CONFIG_DIR",
		"CODEX_HOME",
	} {
		t.Setenv(name, "")
	}
}
