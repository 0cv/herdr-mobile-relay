//go:build herdr_sqlite_logical_test && herdr_installed_inert_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package sqliteexec

import "context"

// The inert app fixture constructs the real owned executor/Reader without
// discovering a CLI, opening SQLite, or claiming that SQL is operational.
// All SQL attempts fail closed. Ordinary logical selections remain unchanged.
func productionInstalledDeps() installedDeps {
	return installedDeps{
		nativeReady: func(context.Context) bool { return false },
		lookup: func(binary string) (string, error) {
			if binary != "sqlite3" {
				return "", ErrUnavailable
			}
			return binary, nil
		},
		open: func(string) (installedDB, error) { return nil, ErrUnavailable },
		run: func(context.Context, string, string, string, int) ([]byte, error) {
			return nil, ErrUnavailable
		},
	}
}
