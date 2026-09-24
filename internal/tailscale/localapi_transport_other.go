//go:build (!darwin && !linux) || android || ios

package tailscale

import (
	"context"
	"net"
)

func platformLocalAPIDialer() (func(context.Context, string, string) (net.Conn, error), bool, error) {
	return nil, true, unsupportedPlatformError()
}
