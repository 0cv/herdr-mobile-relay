//go:build !herdr_tailscale_test

package tailscalecli

import "runtime"

func fixtureCLIExecutableRequired() bool { return false }

func profilePlatform() (string, string) { return runtime.GOOS, runtime.GOARCH }
