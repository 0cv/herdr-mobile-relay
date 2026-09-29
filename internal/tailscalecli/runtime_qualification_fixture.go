//go:build herdr_tailscale_test

package tailscalecli

// This compile-time hook is used only by a disposable hosted fixture binary.
// Its harness supplies a synthetic CLI in an isolated container; release
// archives never use this tag or claim live runtime qualification.
func fixtureRuntimeQualificationEnabled() bool { return true }
