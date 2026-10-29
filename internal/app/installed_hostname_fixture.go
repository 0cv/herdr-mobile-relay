//go:build herdr_installed_inert_test

package app

// The inert constructor does not read host metadata before runner injection.
func installedConstructorHostname() (string, error) { return "relay", nil }
