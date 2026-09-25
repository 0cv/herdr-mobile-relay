//go:build !herdr_tailscale_test

package app

import (
	"net/http"
	"time"
)

// The production build always uses the fixed system-trust, proxy-free client.
// The hosted test build supplies its isolated fixture-CA variant from the
// tagged app test file instead; no runtime client injection is shipped.
func managedHealthClientForServer(_ *Server, timeout time.Duration) *http.Client {
	return managedHealthClient(timeout)
}
