//go:build !herdr_tailscale_test || herdr_tailscale_cli_fixture_binary

package app

import (
	"net/http"
	"time"
)

// Release builds use the fixed system-trust, proxy-free client. Only the
// explicitly tagged foreground-command fixture constructor can attach an
// isolated test client to one server instance.
func managedHealthClientForServer(server *Server, timeout time.Duration) *http.Client {
	if server != nil && server.developmentCLIHealthClient != nil {
		return server.developmentCLIHealthClient(timeout)
	}
	return managedHealthClient(timeout)
}
