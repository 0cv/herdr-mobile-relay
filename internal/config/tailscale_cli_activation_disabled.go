//go:build !herdr_tailscale_test

package config

// Shipped builds keep production CLI profile activation disabled until
// physical-phone qualification and separate production enablement.
const tailscaleCLIProfilesEnabled = false
