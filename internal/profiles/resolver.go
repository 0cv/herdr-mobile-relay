package profiles

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

type Profile struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Kind  string   `json:"-"`
	Argv  []string `json:"-"`
}

var defaultCandidates = []Profile{
	{ID: "codex", Label: "Codex", Kind: "codex"},
	{ID: "claude", Label: "Claude Code", Kind: "claude"},
	{ID: "opencode", Label: "OpenCode", Kind: "opencode"},
	{ID: "pi", Label: "Pi", Kind: "pi"},
	{ID: "omp", Label: "Oh My Pi", Kind: "omp"},
	{ID: "kimi", Label: "Kimi", Kind: "kimi"},
}

var defaultAliases = map[string]string{
	"claude-code":     "claude",
	"claude code":     "claude",
	"pi-coding-agent": "pi",
}

// defaultSkillDirs and defaultCommandFormats hold no defaults: every provider
// discovers its own skill directories the way its agent does, and a default
// format would make CommandConfig report every such profile as explicitly
// configured, permanently routing it down the INI escape hatch. Both maps stay
// because the INI [skills] and [commands] sections populate them at runtime.
var defaultSkillDirs = map[string][]string{}

var defaultCommandFormats = map[string]string{}

type IntegrationStatuser interface {
	IntegrationStatus(ctx context.Context) ([]byte, error)
}

type Resolver struct {
	mu         sync.Mutex
	cached     []Profile
	expires    time.Time
	configHome string
	herdr      IntegrationStatuser
	remembered map[string]string
	aliases    map[string]string
	skillDirs  map[string][]string
	formats    map[string]string
	warned     map[string]bool
	versions   map[string]cachedVersion
}

type cachedVersion struct {
	value   string
	expires time.Time
}

var semanticVersionPattern = regexp.MustCompile(`\b[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?\b`)

func NewResolver(configHome string, herdr IntegrationStatuser) *Resolver {
	return &Resolver{
		configHome: configHome,
		herdr:      herdr,
		remembered: make(map[string]string),
		aliases:    cloneAliases(defaultAliases),
		skillDirs:  cloneStringSlices(defaultSkillDirs),
		formats:    cloneStrings(defaultCommandFormats),
		warned:     make(map[string]bool),
		versions:   make(map[string]cachedVersion),
	}
}

func (r *Resolver) Profiles() []Profile {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Now().Before(r.expires) {
		return cloneProfiles(r.cached)
	}

	profiles := r.discover()
	r.cached = profiles
	r.expires = time.Now().Add(5 * time.Minute)
	return cloneProfiles(profiles)
}

func (r *Resolver) discover() []Profile {
	configured, replace, aliases, skillDirs, formats := r.loadINI()
	r.aliases = aliases
	r.skillDirs = skillDirs
	r.formats = formats

	seen := make(map[string]bool)
	configuredIDs := make(map[string]bool, len(configured))
	for _, profile := range configured {
		configuredIDs[profile.ID] = true
	}
	var result []Profile

	candidates := configured
	if !replace {
		candidates = mergeProfiles(defaultCandidates, configured)
	}
	if len(candidates) == 0 {
		candidates = append([]Profile(nil), defaultCandidates...)
	}
	for _, p := range candidates {
		path, ok := binaryPath(p.ID)
		if !ok {
			if configuredIDs[p.ID] {
				r.warnOnce("missing-profile:"+p.ID, "configured agent profile executable is unavailable", "profile_id", p.ID)
			}
			continue
		}
		if !seen[p.ID] {
			p.Argv = []string{path}
			if isHerdrKind(p.ID) {
				p.Kind = p.ID
			}
			seen[p.ID] = true
			result = append(result, p)
		}
	}

	if !replace {
		for _, p := range r.discoverIntegrations() {
			if !seen[p.ID] {
				if path, ok := binaryPath(p.ID); ok {
					p.Argv = []string{path}
				}
				p.Kind = p.ID
				seen[p.ID] = true
				result = append(result, p)
			}
		}
	}

	return result
}

func (r *Resolver) loadINI() (
	profiles []Profile,
	replace bool,
	aliases map[string]string,
	skillDirs map[string][]string,
	formats map[string]string,
) {
	aliases = cloneAliases(defaultAliases)
	skillDirs = cloneStringSlices(defaultSkillDirs)
	formats = cloneStrings(defaultCommandFormats)
	iniPath := filepath.Join(r.configHome, "herdr", "agent-profiles.ini")
	data, err := os.ReadFile(iniPath)
	if err != nil {
		return
	}

	ini, err := config.ParseINI(strings.NewReader(string(data)))
	if err != nil {
		return
	}

	if value, ok := ini.Get("config", "replace_profiles"); ok {
		replace = parseBool(value)
	}

	if section, ok := ini.Sections["profiles"]; ok {
		ids := sortedKeys(section)
		for _, id := range ids {
			label := strings.TrimSpace(section[id])
			id = strings.ToLower(strings.TrimSpace(id))
			if id != "" && label != "" {
				profiles = append(profiles, Profile{ID: id, Label: label})
			}
		}
	}
	if section, ok := ini.Sections["aliases"]; ok {
		for name, profileID := range section {
			name = strings.ToLower(strings.TrimSpace(name))
			profileID = strings.ToLower(strings.TrimSpace(profileID))
			if name != "" && profileID != "" {
				aliases[name] = profileID
			}
		}
	}
	if section, ok := ini.Sections["skills"]; ok {
		for profileID, raw := range section {
			var paths []string
			for _, value := range filepath.SplitList(raw) {
				value = strings.TrimSpace(value)
				if value == "" {
					continue
				}
				if value == "~" || strings.HasPrefix(value, "~/") {
					if home, homeErr := os.UserHomeDir(); homeErr == nil {
						value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
					}
				}
				paths = append(paths, value)
			}
			if len(paths) > 0 {
				skillDirs[strings.ToLower(strings.TrimSpace(profileID))] = paths
			}
		}
	}
	if section, ok := ini.Sections["commands"]; ok {
		for profileID, format := range section {
			profileID = strings.ToLower(strings.TrimSpace(profileID))
			format = strings.TrimSpace(format)
			formats[profileID] = format
			if format != "" && !strings.EqualFold(format, "off") && !validCommandFormat(format) {
				r.warnOnce("invalid-command-format:"+profileID, "configured agent command format is invalid and was disabled", "profile_id", profileID)
			}
		}
	}
	return
}

var integrationLabels = map[string]string{
	"qodercli": "Qoder",
}

func (r *Resolver) discoverIntegrations() []Profile {
	if r.herdr == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := r.herdr.IntegrationStatus(ctx)
	if err != nil {
		return nil
	}

	var result []Profile
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		rest := strings.TrimSpace(parts[1])
		if !strings.HasPrefix(rest, "current") && !strings.HasPrefix(rest, "outdated") {
			continue
		}
		label := id
		if mapped, ok := integrationLabels[strings.ToLower(id)]; ok {
			label = mapped
		}
		result = append(result, Profile{ID: id, Label: label, Kind: id})
	}
	return result
}

func (r *Resolver) ProfileIDForAgent(agent string) string {
	agentLower := strings.ToLower(strings.TrimSpace(agent))
	profiles := r.Profiles()

	r.mu.Lock()
	alias := r.aliases[agentLower]
	r.mu.Unlock()

	for _, p := range profiles {
		if agentLower == strings.ToLower(p.ID) {
			return p.ID
		}
	}
	for _, p := range profiles {
		if alias == p.ID {
			return alias
		}
	}
	return ""
}

func (r *Resolver) Profile(id string) (Profile, bool) {
	for _, profile := range r.Profiles() {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

func (r *Resolver) Remember(paneID, profileID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remembered[strings.ToLower(strings.TrimSpace(paneID))] = profileID
}

func (r *Resolver) Forget(paneID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.remembered, strings.ToLower(strings.TrimSpace(paneID)))
}

func (r *Resolver) ResolvePane(paneID, reportedAgent string) string {
	r.mu.Lock()
	if id := r.remembered[strings.ToLower(strings.TrimSpace(paneID))]; id != "" {
		r.mu.Unlock()
		return id
	}
	r.mu.Unlock()
	return r.ProfileIDForAgent(reportedAgent)
}

func (r *Resolver) Reload() {
	r.mu.Lock()
	r.cached = nil
	r.expires = time.Time{}
	r.versions = make(map[string]cachedVersion)
	r.mu.Unlock()
}

func (r *Resolver) AgentVersion(profileID string) string {
	profileID = strings.ToLower(strings.TrimSpace(profileID))
	if profileID == "" {
		return ""
	}
	r.mu.Lock()
	if cached, ok := r.versions[profileID]; ok && time.Now().Before(cached.expires) {
		r.mu.Unlock()
		return cached.value
	}
	r.mu.Unlock()

	profile, ok := r.Profile(profileID)
	if !ok || len(profile.Argv) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, profile.Argv[0], "--version").CombinedOutput()
	version := ""
	if err == nil {
		version = semanticVersionPattern.FindString(string(output))
	}
	r.mu.Lock()
	r.versions[profileID] = cachedVersion{value: version, expires: time.Now().Add(5 * time.Minute)}
	r.mu.Unlock()
	return version
}

func (r *Resolver) CommandConfig(profileID string) ([]string, string, bool) {
	// Profiles() refreshes all INI-derived settings when the cache expired or
	// Reload cleared it, keeping profiles, aliases, skills, and formats on one
	// configuration generation.
	_ = r.Profiles()
	profileID = strings.ToLower(strings.TrimSpace(profileID))
	r.mu.Lock()
	defer r.mu.Unlock()
	format, configured := r.formats[profileID]
	format = strings.TrimSpace(format)
	if !configured || format == "" || strings.EqualFold(format, "off") || !validCommandFormat(format) {
		return nil, "", false
	}
	return expandedPaths(r.skillDirs[profileID]), format, true
}

func (r *Resolver) warnOnce(key, message string, args ...any) {
	if r.warned[key] {
		return
	}
	r.warned[key] = true
	slog.Warn(message, args...)
}

func expandedPaths(paths []string) []string {
	result := append([]string(nil), paths...)
	home, err := os.UserHomeDir()
	if err != nil {
		return result
	}
	for index, value := range result {
		if value == "~" {
			result[index] = home
		} else if strings.HasPrefix(value, "~/") {
			result[index] = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	return result
}

func binaryPath(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

func cloneAliases(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneStringSlices(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for key, value := range source {
		result[key] = append([]string(nil), value...)
	}
	return result
}

func cloneProfiles(source []Profile) []Profile {
	result := make([]Profile, len(source))
	for i, profile := range source {
		result[i] = profile
		result[i].Argv = append([]string(nil), profile.Argv...)
	}
	return result
}

func mergeProfiles(defaults, configured []Profile) []Profile {
	result := append([]Profile(nil), defaults...)
	positions := make(map[string]int, len(result))
	for i, profile := range result {
		positions[profile.ID] = i
	}
	for _, profile := range configured {
		if index, ok := positions[profile.ID]; ok {
			result[index].Label = profile.Label
			continue
		}
		positions[profile.ID] = len(result)
		result = append(result, profile)
	}
	return result
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "1", "on":
		return true
	default:
		return false
	}
}

func validCommandFormat(value string) bool {
	return strings.Count(value, "{name}") == 1 &&
		!strings.Contains(strings.Replace(value, "{name}", "", 1), "{") &&
		!strings.Contains(strings.Replace(value, "{name}", "", 1), "}")
}

func isHerdrKind(id string) bool {
	switch id {
	case "agy", "amp", "claude", "cline", "codex", "copilot", "cursor", "devin",
		"droid", "gemini", "grok", "hermes", "kilo", "kimi", "kiro", "maki",
		"mastracode", "omp", "opencode", "pi", "qodercli":
		return true
	default:
		return false
	}
}
