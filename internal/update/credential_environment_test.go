package update

import (
	"maps"
	"strings"
	"testing"
)

func TestUpdaterRetainsPrivateFileWithoutRawTokens(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-raw-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-raw-token")
	t.Setenv("HERDR_GITHUB_TOKEN_FILE", "/private/updater-file")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	environment := strings.Join(environmentWith("HERDR_MOBILE_RELAY_NO_AUTO_SETUP", "1"), "\n")
	if strings.Contains(environment, "synthetic-raw-token") {
		t.Fatal("raw credential inherited by updater subprocess")
	}
	for _, expected := range []string{"HERDR_GITHUB_TOKEN_FILE=/private/updater-file", "HTTPS_PROXY=http://proxy.invalid"} {
		if !strings.Contains(environment, expected) {
			t.Fatal("updater lost required environment")
		}
	}
	job := updateWorkerJob("/relay", "/job", func(key string) (string, bool) {
		if key == "HERDR_GITHUB_TOKEN_FILE" {
			return "/private/updater-file", true
		}
		if key == "GH_TOKEN" || key == "GITHUB_TOKEN" {
			return "synthetic-raw-token", true
		}
		return "", false
	})
	if !maps.Equal(job.Environment, map[string]string{"HERDR_GITHUB_TOKEN_FILE": "/private/updater-file"}) {
		t.Fatal("incorrect updater launchd environment")
	}
	args := strings.Join(systemdRunArgs(job), "\n")
	if !strings.Contains(args, "HERDR_GITHUB_TOKEN_FILE=/private/updater-file") || strings.Contains(args, "synthetic-raw-token") {
		t.Fatal("incorrect updater systemd-run environment")
	}
}
