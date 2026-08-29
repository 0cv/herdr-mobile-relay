package slashcmd

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

type ompExtensionDir struct {
	path   string
	source string
}

type ompExtensionSettings struct {
	Extensions []string `json:"extensions"`
}

// ompExtensionSkillDirs resolves the extension roots OMP can load without CLI
// process state: project and user settings, plus installed package dependencies
// in the corresponding extension roots. CLI-only --extension flags are not
// visible to the relay process.
func ompExtensionSkillDirs(ctx DiscoverContext) []ompExtensionDir {
	var roots []ompExtensionDir
	projectDirs := findProjectDirs(ctx.Cwd, []string{".omp"})
	for index := len(projectDirs) - 1; index >= 0; index-- {
		dir := projectDirs[index]
		roots = append(roots, configuredOMPExtensions(ctx, dir, "project")...)
		roots = append(roots, installedOMPExtensions(dir, "project")...)
	}

	agentDir := selectedAgentDir(ctx, ".omp", "HERDR_OMP_CONFIG_DIRS", "PI_CODING_AGENT_DIR")
	if agentDir != "" {
		roots = append(roots, configuredOMPExtensions(ctx, agentDir, "personal")...)
		roots = append(roots, installedOMPExtensions(agentDir, "personal")...)
	}

	seen := make(map[string]bool, len(roots))
	unique := roots[:0]
	for _, root := range roots {
		clean := filepath.Clean(root.path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		root.path = clean
		unique = append(unique, root)
	}
	return unique
}

func configuredOMPExtensions(ctx DiscoverContext, settingsDir, source string) []ompExtensionDir {
	data, found, ok := settingsFileIn(settingsDir, "settings.json")
	if !found || !ok {
		return nil
	}
	var settings ompExtensionSettings
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	roots := make([]ompExtensionDir, 0, len(settings.Extensions))
	for _, extension := range settings.Extensions {
		extension = expandTilde(strings.TrimSpace(extension), ctx.Home)
		if !filepath.IsAbs(extension) {
			if ctx.Cwd == "" {
				continue
			}
			extension = filepath.Join(ctx.Cwd, extension)
		}
		roots = append(roots, ompExtensionDir{path: extension, source: source})
	}
	return roots
}

type ompPackageManifest struct {
	Dependencies map[string]string `json:"dependencies"`
	OMP          json.RawMessage   `json:"omp"`
	Pi           json.RawMessage   `json:"pi"`
}

type ompPluginLock struct {
	Plugins map[string]struct {
		Enabled *bool `json:"enabled"`
	} `json:"plugins"`
}

func installedOMPExtensions(root, source string) []ompExtensionDir {
	candidates := make(map[string]bool)
	if data, found, ok := settingsFileIn(root, "package.json"); found && ok {
		var manifest ompPackageManifest
		if json.Unmarshal(data, &manifest) == nil {
			for name := range manifest.Dependencies {
				candidates[name] = true
			}
		}
	}
	if data, found, ok := settingsFileIn(root, "omp-plugins.lock.json"); found && ok {
		var lock ompPluginLock
		if json.Unmarshal(data, &lock) == nil {
			for name, plugin := range lock.Plugins {
				candidates[name] = plugin.Enabled == nil || *plugin.Enabled
			}
		}
	}

	names := make([]string, 0, len(candidates))
	for name, enabled := range candidates {
		if enabled && validPackageName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]ompExtensionDir, 0, len(names))
	for _, name := range names {
		packageRoot := filepath.Join(root, "node_modules", filepath.FromSlash(name))
		data, found, ok := settingsFileIn(packageRoot, "package.json")
		if !found || !ok {
			continue
		}
		var manifest ompPackageManifest
		if json.Unmarshal(data, &manifest) != nil || (len(manifest.OMP) == 0 && len(manifest.Pi) == 0) {
			continue
		}
		result = append(result, ompExtensionDir{path: packageRoot, source: source})
	}
	return result
}

func validPackageName(name string) bool {
	if name == "" || filepath.IsAbs(name) || strings.Contains(name, "\\") {
		return false
	}
	parts := strings.Split(name, "/")
	if len(parts) > 2 || (len(parts) == 2 && !strings.HasPrefix(parts[0], "@")) {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
