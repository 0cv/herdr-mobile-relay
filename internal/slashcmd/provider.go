package slashcmd

import "strings"

type Provider interface {
	ID() string
	Discover(ctx DiscoverContext) ([]Command, bool)
}

type DiscoverContext struct {
	Cwd           string
	Home          string
	SkillDirs     []string
	CommandFormat string
	AgentVersion  string
}

var providers = map[string]Provider{}

func registerProvider(p Provider) {
	providers[p.ID()] = p
}

func resolveProvider(profileID string) Provider {
	return providers[strings.ToLower(strings.TrimSpace(profileID))]
}
