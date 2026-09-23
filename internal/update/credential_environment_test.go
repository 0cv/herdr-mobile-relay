package update

import (
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
	for _, platform := range []string{"linux", "darwin"} {
		launch := updateWorkerLaunch(platform, "test", "/relay", "/job", func(key string) (string, bool) {
			if key == "HERDR_GITHUB_TOKEN_FILE" {
				return "/private/updater-file", true
			}
			if key == "GH_TOKEN" || key == "GITHUB_TOKEN" {
				return "synthetic-raw-token", true
			}
			return "", false
		})
		args := strings.Join(launch.args, "\n")
		if !strings.Contains(args, "HERDR_GITHUB_TOKEN_FILE=/private/updater-file") || strings.Contains(args, "synthetic-raw-token") {
			t.Fatalf("incorrect updater launch environment for %s", platform)
		}
	}
}
