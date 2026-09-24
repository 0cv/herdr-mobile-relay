//go:build darwin && !ios

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
	const supportedSocket = "/var/run/tailscaled.socket"
	if runtime.GOOS != "darwin" || paths.DefaultTailscaledSocket() != supportedSocket {
		return nil, true, unsupportedPlatformError()
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != localAPIHost+":80" {
			return nil, fmt.Errorf("invalid local Tailscale dial target")
		}
		return safesocket.ConnectContext(ctx, supportedSocket)
	}, true, nil
}
