//go:build herdr_tailscale_test

package config

// The fixture build tag never enables production profile activation. Hosted
// fixtures use synthetic CLI executables with an explicit marker instead.
const tailscaleCLIProfilesEnabled = false
