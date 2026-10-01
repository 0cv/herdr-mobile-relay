package childenv

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

func WithoutReleaseCredentials(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, variable := range environment {
		key, _, _ := strings.Cut(variable, "=")
		switch key {
		case "GH_TOKEN", "GITHUB_TOKEN", "HERDR_GITHUB_TOKEN_FILE":
			continue
		}
		filtered = append(filtered, variable)
	}
	return filtered
}

func Command(name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	command.Env = WithoutReleaseCredentials(os.Environ())
	return command
}

func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = WithoutReleaseCredentials(os.Environ())
	return command
}
