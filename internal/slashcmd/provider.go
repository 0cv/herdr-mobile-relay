package slashcmd

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

type Provider interface {
	ID() string
	Discover(ctx DiscoverContext) ([]Command, bool)
}

type DiscoverContext struct {
	AgentDir       string
	Cwd            string
	Home           string
	SkillDirs      []string
	CommandFormat  string
	SuppressNative bool
}

var providers = map[string]Provider{}

func registerProvider(p Provider) {
	providers[p.ID()] = p
}

func resolveProvider(profileID string) Provider {
	return providers[strings.ToLower(strings.TrimSpace(profileID))]
}

// selectedAgentDir chooses one pane-scoped Pi/OMP agent directory. It never
// merges named profiles: a transcript-derived AgentDir wins, followed by the
// first explicit service override, then the home default.
func selectedAgentDir(ctx DiscoverContext, configName, listEnv, singleEnv string) string {
	if filepath.IsAbs(ctx.AgentDir) {
		return filepath.Clean(ctx.AgentDir)
	}
	for _, candidate := range filepath.SplitList(os.Getenv(listEnv)) {
		candidate = agentroots.ExpandTilde(candidate, ctx.Home)
		if filepath.IsAbs(candidate) {
			return filepath.Clean(candidate)
		}
	}
	candidate := agentroots.ExpandTilde(os.Getenv(singleEnv), ctx.Home)
	if filepath.IsAbs(candidate) {
		return filepath.Clean(candidate)
	}
	candidate = filepath.Join(ctx.Home, configName, "agent")
	if filepath.IsAbs(candidate) {
		return candidate
	}
	return ""
}
