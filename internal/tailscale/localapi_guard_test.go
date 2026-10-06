package tailscale

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
)

var guardedAppStoreDiscoverCalls int
var guardedLoopbackDialCalls int

func TestMain(m *testing.M) {
	previousDiscover := appStoreDiscover
	previousDial := dialLoopbackTCP
	appStoreDiscover = func() (int, string, error) {
		guardedAppStoreDiscoverCalls++
		panic("real App Store discovery is forbidden in tests")
	}
	dialLoopbackTCP = func(context.Context, string, string) (net.Conn, error) {
		guardedLoopbackDialCalls++
		panic("real App Store loopback dialing is forbidden in tests")
	}

	code := m.Run()
	appStoreDiscover = previousDiscover
	dialLoopbackTCP = previousDial
	if guardedAppStoreDiscoverCalls != 0 || guardedLoopbackDialCalls != 0 {
		fmt.Fprintf(os.Stderr, "forbidden upstream App Store seams invoked: discovery=%d dial=%d\n", guardedAppStoreDiscoverCalls, guardedLoopbackDialCalls)
		code = 1
	}
	os.Exit(code)
}
