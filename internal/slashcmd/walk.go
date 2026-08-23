package slashcmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxWalkFiles    = 250
	maxGitWalkDepth = 32
)

func walkCommandDir(root, source string) []Command {
	budget := maxWalkFiles
	var suppressed []string
	var commands []Command
	truncated := false
	walkDirBudget(root, "", source, &commands, &suppressed, &budget, &truncated)
	return commands
}

func walkCommandDirBudget(root, source string, budget *int) ([]Command, []string, bool) {
	var commands []Command
	var suppressed []string
	truncated := false
	walkDirBudget(root, "", source, &commands, &suppressed, budget, &truncated)
	return commands, suppressed, truncated
}

func walkDirBudget(dir, namespace, source string, out *[]Command, suppressed *[]string, budget *int, truncated *bool) {
	if *budget <= 0 {
		*truncated = true
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if *budget <= 0 {
			*truncated = true
			return
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		fullPath := filepath.Join(dir, name)
		if e.IsDir() {
			skillFile := filepath.Join(fullPath, "SKILL.md")
			if info, err := os.Stat(skillFile); err == nil && info.Mode().IsRegular() {
				*budget--
				if cmd, suppressedName := parseSkillEntry(skillFile, name, namespace, source); cmd != nil {
					*out = append(*out, *cmd)
				} else if suppressedName != "" {
					*suppressed = append(*suppressed, suppressedName)
				}
				continue
			}
			childNS := name
			if namespace != "" {
				childNS = namespace + ":" + name
			}
			walkDirBudget(fullPath, childNS, source, out, suppressed, budget, truncated)
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		cmdName := strings.TrimSuffix(name, ".md")
		if !commandNamePattern.MatchString(cmdName) {
			continue
		}
		*budget--
		fm := fileFrontmatter(fullPath)
		if isHidden(fm) {
			continue
		}
		fullName := "/" + cmdName
		if namespace != "" {
			fullName = "/" + namespace + ":" + cmdName
		}
		if !userInvocable(fm) {
			*suppressed = append(*suppressed, fullName)
			continue
		}
		*out = append(*out, Command{
			Command:      fullName,
			Description:  descriptionFrom(fm, fullPath),
			Source:       source,
			ArgumentHint: compact(fm["argument-hint"], 120),
		})
	}
}

func parseSkillEntry(skillFile, dirName, namespace, source string) (*Command, string) {
	metadata, ok := readSkillMetadata(skillFile)
	if !ok {
		return nil, ""
	}
	name := metadata["name"]
	if name == "" {
		name = dirName
	}
	if !commandNamePattern.MatchString(name) {
		return nil, ""
	}
	fullName := "/" + name
	if namespace != "" {
		fullName = "/" + namespace + ":" + name
	}
	if !userInvocable(metadata) {
		return nil, fullName
	}
	description := metadata["description"]
	if description == "" {
		description = strings.ToUpper(name[:1]) + name[1:] + " skill"
	}
	return &Command{
		Command:      fullName,
		Description:  compact(description, 240),
		Source:       source,
		ArgumentHint: compact(metadata["argument-hint"], 120),
	}, ""
}

func findGitRoot(dir string) string {
	current := dir
	for depth := 0; depth < maxGitWalkDepth; depth++ {
		candidate := filepath.Join(current, ".git")
		if _, err := os.Lstat(candidate); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func scanSkillDir(dir, source string) []Command {
	budget := maxWalkFiles
	cmds, _, _ := scanSkillDirBudget(dir, source, &budget)
	return cmds
}

func scanSkillDirBudget(dir, source string, budget *int) ([]Command, []string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, false
	}
	var commands []Command
	var suppressed []string
	truncated := false
	for _, e := range entries {
		if *budget <= 0 {
			truncated = true
			break
		}
		if !e.IsDir() {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		skillFile := filepath.Join(dir, e.Name(), "SKILL.md")
		info, err := os.Stat(skillFile)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		*budget--
		if cmd, suppressedName := parseSkillEntry(skillFile, e.Name(), "", source); cmd != nil {
			commands = append(commands, *cmd)
		} else if suppressedName != "" {
			suppressed = append(suppressed, suppressedName)
		}
	}
	return commands, suppressed, truncated
}

// scanSkillDirFormat scans dir for <entry>/SKILL.md and renders each skill
// through format, which must contain exactly one "{name}". The skill name comes
// from frontmatter "name", falling back to the directory name, matching how omp,
// Pi and Kimi resolve it. Skills without a description are dropped, as those
// agents require one. Reports whether budget ran out.
//
// Unlike scanSkillDirBudget this follows symlinked skill directories and
// de-duplicates by real path, matching omp, and returns no suppression list:
// none of omp, Pi or Kimi has a frontmatter field that hides a skill from the
// command palette.
func scanSkillDirFormat(dir, source, format string, budget *int) ([]Command, bool) {
	if strings.Count(format, "{name}") != 1 {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var commands []Command
	seen := make(map[string]bool, len(entries))
	truncated := false
	for _, e := range entries {
		if *budget <= 0 {
			truncated = true
			break
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		skillDir := filepath.Join(dir, e.Name())
		if info, err := os.Stat(skillDir); err != nil || !info.IsDir() {
			continue
		}
		skillFile := filepath.Join(skillDir, "SKILL.md")
		info, err := os.Stat(skillFile)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		real, err := filepath.EvalSymlinks(skillFile)
		if err != nil {
			real = skillFile
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		*budget--
		metadata, ok := readSkillMetadata(skillFile)
		if !ok {
			continue
		}
		name := metadata["name"]
		if name == "" {
			name = e.Name()
		}
		if !commandNamePattern.MatchString(name) {
			continue
		}
		description := metadata["description"]
		if description == "" {
			continue
		}
		commands = append(commands, Command{
			Command:      "/" + strings.TrimPrefix(strings.Replace(format, "{name}", name, 1), "/"),
			Description:  compact(description, 240),
			Source:       source,
			ArgumentHint: compact(metadata["argument-hint"], 120),
		})
	}
	return commands, truncated
}

func fileFrontmatter(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxMetadataSize {
		return map[string]string{}
	}
	fm, _ := parseFrontmatterBytes(data)
	return fm
}

func descriptionFrom(fm map[string]string, path string) string {
	if desc := fm["description"]; desc != "" {
		return compact(desc, 120)
	}
	return extractFirstLine(path)
}

func isHidden(fm map[string]string) bool {
	switch strings.ToLower(strings.TrimSpace(fm["hidden"])) {
	case "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

func extractFirstLine(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxMetadataSize {
		return ""
	}
	lines := strings.SplitN(string(data), "\n", 10)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "---") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return compact(trimmed, 120)
	}
	return ""
}
