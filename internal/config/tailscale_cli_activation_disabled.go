//go:build !herdr_tailscale_test

package config

// Shipped builds keep CLI profile activation disabled until P6.
const tailscaleCLIProfilesEnabled = false
