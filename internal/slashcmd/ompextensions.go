package slashcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ompExtensionDir struct {
	path           string
	source         string
	directSkillDir bool
}

// ompExtensionSkillDirs resolves configured extension packages and enabled
// plugins. Only the nearest project .omp root participates.
func ompExtensionSkillDirs(ctx DiscoverContext, settings ompSkillSettings) []ompExtensionDir {
	var roots []ompExtensionDir
	configuredSource := settings.extensionDirectorySource
	if configuredSource == "" {
		configuredSource = "personal"
	}
	for _, extension := range settings.extensionDirectories {
		extension = expandTilde(strings.TrimSpace(extension), ctx.Home)
		if extension == "" || strings.HasPrefix(extension, "~") {
			continue
		}
		if !filepath.IsAbs(extension) {
			if !filepath.IsAbs(ctx.Cwd) {
				continue
			}
			extension = filepath.Join(ctx.Cwd, extension)
		}
		roots = append(roots, ompExtensionDir{path: extension, source: configuredSource})
	}

	disabled := ompDisabledPlugins(ctx.Cwd)
	projectDirs := findProjectDirs(ctx.Cwd, []string{".omp"})
	if len(projectDirs) > 0 {
		dir := projectDirs[len(projectDirs)-1]
		roots = append(roots, claudeMarketplaceExtensions(ctx, filepath.Join(dir, "plugins", "installed_plugins.json"), "project")...)
		roots = append(roots, installedOMPExtensions(filepath.Join(dir, "plugins"), "project", disabled)...)
	}

	if ctx.Home != "" {
		roots = append(roots, claudeMarketplaceExtensions(ctx, filepath.Join(ctx.Home, ".claude", "plugins", "installed_plugins.json"), "personal")...)
		roots = append(roots, claudeMarketplaceExtensions(ctx, filepath.Join(ctx.Home, ".omp", "plugins", "installed_plugins.json"), "personal")...)
		roots = append(roots, installedOMPExtensions(filepath.Join(ctx.Home, ".omp", "plugins"), "personal", disabled)...)
	}

	seen := make(map[string]bool, len(roots))
	unique := roots[:0]
	for _, root := range roots {
		clean := filepath.Clean(root.path)
		key := clean
		if root.directSkillDir {
			key += "\x00direct"
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		root.path = clean
		unique = append(unique, root)
	}
	return unique
}

type claudeMarketplaceRegistry struct {
	Plugins map[string][]struct {
		InstallPath string `json:"installPath"`
		Enabled     *bool  `json:"enabled"`
		Scope       string `json:"scope"`
		ProjectPath string `json:"projectPath"`
	} `json:"plugins"`
}

func claudeMarketplaceExtensions(ctx DiscoverContext, registryPath, source string) []ompExtensionDir {
	data, found, ok := settingsFileIn(filepath.Dir(registryPath), filepath.Base(registryPath))
	if !found || !ok {
		return nil
	}
	var registry claudeMarketplaceRegistry
	if json.Unmarshal(data, &registry) != nil {
		return nil
	}
	var roots []ompExtensionDir
	for _, installs := range registry.Plugins {
		for _, install := range installs {
			if install.Enabled != nil && !*install.Enabled {
				continue
			}
			root := expandTilde(strings.TrimSpace(install.InstallPath), ctx.Home)
			if root == "" || !filepath.IsAbs(root) {
				continue
			}
			if install.Scope == "project" || install.Scope == "local" {
				if install.ProjectPath == "" {
					if source != "project" {
						continue
					}
				} else if !pathWithin(ctx.Cwd, expandTilde(install.ProjectPath, ctx.Home)) {
					continue
				}
			}
			roots = append(roots, claudeMarketplaceSkillDirs(root, source)...)
		}
	}
	return roots
}

type claudePluginManifest struct {
	Skills json.RawMessage `json:"skills"`
}

func claudeMarketplaceSkillDirs(root, source string) []ompExtensionDir {
	result := []ompExtensionDir{{path: root, source: source}}
	data, found, ok := settingsFileIn(filepath.Join(root, ".claude-plugin"), "plugin.json")
	if !found || !ok {
		return result
	}
	var manifest claudePluginManifest
	if json.Unmarshal(data, &manifest) != nil || len(manifest.Skills) == 0 {
		return result
	}
	var configured []string
	var single string
	if json.Unmarshal(manifest.Skills, &single) == nil {
		configured = []string{single}
	} else if json.Unmarshal(manifest.Skills, &configured) != nil {
		return result
	}
	for _, skillDir := range configured {
		skillDir = strings.TrimSpace(skillDir)
		if skillDir == "" || strings.HasPrefix(skillDir, "~") {
			continue
		}
		if !filepath.IsAbs(skillDir) {
			skillDir = filepath.Join(root, skillDir)
		}
		if !pathWithin(skillDir, root) {
			continue
		}
		result = append(result, ompExtensionDir{path: skillDir, source: source, directSkillDir: true})
	}
	return result
}

func pathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = resolved
	}
	if resolved, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = resolved
	}
	relative, err := filepath.Rel(cleanRoot, cleanPath)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
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

func installedOMPExtensions(root, source string, disabled map[string]bool) []ompExtensionDir {
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
		if enabled && !disabled[name] && validPackageName(name) {
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

type ompProjectPluginOverrides struct {
	Disabled []string `json:"disabled"`
}

func ompDisabledPlugins(cwd string) map[string]bool {
	disabled := make(map[string]bool)
	if !filepath.IsAbs(cwd) {
		return disabled
	}
	current := filepath.Clean(cwd)
	for {
		for _, stem := range []string{".omp", ".pi"} {
			projectConfig := filepath.Join(current, stem)
			info, err := os.Stat(projectConfig)
			if err != nil || !info.IsDir() {
				continue
			}
			data, found, ok := settingsFileIn(projectConfig, "plugin-overrides.json")
			if found && ok {
				var overrides ompProjectPluginOverrides
				if json.Unmarshal(data, &overrides) == nil {
					for _, name := range overrides.Disabled {
						disabled[name] = true
					}
				}
			}
			return disabled
		}
		parent := filepath.Dir(current)
		if parent == current {
			return disabled
		}
		current = parent
	}
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
