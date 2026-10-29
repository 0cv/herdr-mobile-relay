//go:build herdr_sqlite_logical_test && herdr_installed_inert_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package conversation

import (
	"context"
	"errors"
)

// HoldInstalledRootsForFixture replaces only the already-constructed Reader's
// root discovery effect. The caller must join its work before restoring it.
// No Call authority is exposed and production admission is unchanged.
func (r *Reader) HoldInstalledRootsForFixture(fn func(context.Context, string, string) ([]string, error)) (func(), error) {
	if r == nil || r.installed == nil || fn == nil {
		return nil, errors.New("installed reader fixture unavailable")
	}
	previous := r.installed.files.roots
	r.installed.files.roots = fn
	return func() { r.installed.files.roots = previous }, nil
}
