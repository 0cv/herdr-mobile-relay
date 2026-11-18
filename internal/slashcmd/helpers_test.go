package slashcmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/agentkind"
)

func catalogFor(agent, cwd, home string) Catalog {
	return CatalogFor(agentkind.Of(agent), agent, DiscoverContext{Cwd: cwd, Home: home})
}

func discoverGenericSkills(dirs []string, format string) ([]Command, bool) {
	return newCatalogReader().discoverGenericSkills(dirs, format)
}

func readSkillMetadata(path string) (map[string]string, bool) {
	return newCatalogReader().readSkillMetadata(path)
}

func readCursorCommandFile(path string) ([]byte, bool) {
	return newCatalogReader().readCursorCommandFile(path)
}

func readCursorSkillFile(root, skillDir string, project bool) ([]byte, bool) {
	return newCatalogReader().readCursorSkillFile(root, skillDir, project)
}

func piProjectTrusted(agentDir, cwd, defaultTrust string) bool {
	return newCatalogReader().piProjectTrusted(agentDir, cwd, defaultTrust)
}

func scanSkillDirBudget(dir, source string, budget *int) ([]Command, []string, bool) {
	reader := newCatalogReader()
	commands, suppressed, truncated := reader.scanSkillDirBudget(dir, source, budget)
	return commands, suppressed, truncated || reader.truncated
}

func walkCommandDir(root, source string) []Command {
	reader := newCatalogReader()
	budget := maxCustomFiles
	var suppressed []string
	var commands []Command
	truncated := false
	reader.walkDirBudget(root, "", source, &commands, &suppressed, &budget, &truncated)
	return commands
}

func hasCommand(catalog Catalog, name string) bool {
	_, found := commandByName(catalog, name)
	return found
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
