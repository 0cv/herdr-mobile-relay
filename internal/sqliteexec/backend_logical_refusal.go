//go:build herdr_sqlite_logical_test && !herdr_installed_inert_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package sqliteexec

// Ordinary logical selections retain the incomplete, fail-closed installed seam.
func productionInstalledDeps() installedDeps { return installedDeps{} }
