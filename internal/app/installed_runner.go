//go:build !herdr_installed_inert_test

package app

import "context"

func installedCLIRunner() func(*Server, context.Context) error { return (*Server).Run }
