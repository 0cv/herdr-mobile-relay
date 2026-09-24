//go:build linux && !android

package tailscale

import (
	"context"
	"fmt"
	"net"
	"runtime"

	"tailscale.com/paths"
	"tailscale.com/safesocket"
)

func platformLocalAPIDialer() (func(context.Context, string, string) (net.Conn, error), bool, error) {
	const supportedSocket = "/var/run/tailscale/tailscaled.sock"
	if runtime.GOOS != "linux" || paths.DefaultTailscaledSocket() != supportedSocket {
		return nil, false, unsupportedPlatformError()
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != localAPIHost+":80" {
			return nil, fmt.Errorf("invalid local Tailscale dial target")
		}
		return safesocket.ConnectContext(ctx, supportedSocket)
	}, false, nil
}
