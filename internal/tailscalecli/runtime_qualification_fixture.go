//go:build herdr_tailscale_test

package tailscalecli

// This compile-time hook is used only by a disposable hosted fixture binary.
// It requires an explicit synthetic-CLI marker and models the supplied macOS
// profile while running on hosted Linux. It never marks a profile runtime-
// qualified; release archives never use this tag.
func fixtureCLIExecutableRequired() bool { return true }

func profilePlatform() (string, string) { return "darwin", "arm64" }
