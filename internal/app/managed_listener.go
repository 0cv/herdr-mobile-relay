//go:build !herdr_tailscale_test || herdr_tailscale_cli_fixture_binary

package app

import (
	"net/http"
	"time"
)

// Release builds and the separately tagged hosted CLI fixture app use the
// fixed system-trust, proxy-free client. The lifecycle test binary substitutes
// its isolated fixture-CA variant; no runtime client injection is shipped.
func managedHealthClientForServer(_ *Server, timeout time.Duration) *http.Client {
	return managedHealthClient(timeout)
}
