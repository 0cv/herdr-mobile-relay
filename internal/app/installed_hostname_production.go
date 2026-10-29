//go:build !herdr_installed_inert_test

package app

import "os"

func installedConstructorHostname() (string, error) { return os.Hostname() }
