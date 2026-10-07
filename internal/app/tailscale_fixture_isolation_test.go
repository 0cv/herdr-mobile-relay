//go:build herdr_tailscale_test

package app

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestManagedFixtureDialFailsClosedOutsideOwnedLoopback(t *testing.T) {
	const fixturePort = "43123"
	const ownedEndpoint = "127.0.0.1:54321"
	hostPort := net.JoinHostPort(managedFixtureHost, fixturePort)

	t.Run("unknown-host-no-dial", func(t *testing.T) {
		calls := 0
		dial := managedFixtureDialWith(hostPort, ownedEndpoint, func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		conn, err := dial(context.Background(), "tcp", "unknown.invalid:"+fixturePort)
		if err == nil || conn != nil {
			t.Fatalf("unknown destination result = (%v, %v), want refusal", conn, err)
		}
		if calls != 0 {
			t.Fatalf("dial spy calls = %d, want zero", calls)
		}
	})

	t.Run("wrong-address-no-dial", func(t *testing.T) {
		calls := 0
		dial := managedFixtureDialWith(hostPort, ownedEndpoint, func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		for _, destination := range []string{
			net.JoinHostPort(managedFixtureHost, "43124"),
			net.JoinHostPort("other.tailnet.ts.net", fixturePort),
		} {
			conn, err := dial(context.Background(), "tcp", destination)
			if err == nil || conn != nil {
				t.Errorf("wrong address/port destination %q result = (%v, %v), want refusal", destination, conn, err)
			}
		}
		if calls != 0 {
			t.Fatalf("dial spy calls = %d, want zero", calls)
		}
	})

	t.Run("invalid-endpoint-no-dial", func(t *testing.T) {
		calls := 0
		dial := managedFixtureDialWith(hostPort, "localhost:54321", func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		conn, err := dial(context.Background(), "tcp", hostPort)
		if err == nil || conn != nil {
			t.Fatalf("invalid configured endpoint result = (%v, %v), want refusal", conn, err)
		}
		if calls != 0 {
			t.Fatalf("dial spy calls = %d, want zero", calls)
		}
	})

	t.Run("other-loopback-no-dial", func(t *testing.T) {
		calls := 0
		dial := managedFixtureDialWith(hostPort, ownedEndpoint, func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		conn, err := dial(context.Background(), "tcp", "127.0.0.2:43123")
		if err == nil || conn != nil {
			t.Fatalf("other loopback destination result = (%v, %v), want refusal", conn, err)
		}
		if calls != 0 {
			t.Fatalf("dial spy calls = %d, want zero", calls)
		}
	})

	t.Run("owned-loopback-positive", func(t *testing.T) {
		var calls int
		var gotNetwork, gotAddress string
		client, peer := net.Pipe()
		defer client.Close()
		defer peer.Close()
		dial := managedFixtureDialWith(hostPort, ownedEndpoint, func(_ context.Context, network, address string) (net.Conn, error) {
			calls++
			gotNetwork, gotAddress = network, address
			return client, nil
		})
		conn, err := dial(context.Background(), "tcp", hostPort)
		if err != nil || conn != client {
			t.Fatalf("owned fixture destination result = (%v, %v), want spy connection", conn, err)
		}
		if calls != 1 || gotNetwork != "tcp" || gotAddress != ownedEndpoint {
			t.Fatalf("dial spy received calls=%d network=%q address=%q; want one tcp dial to %q", calls, gotNetwork, gotAddress, ownedEndpoint)
		}
	})
}

func TestManagedFixtureDialRoutesPublicAndBackend(t *testing.T) {
	const publicPort = "43123"
	const backendPort = "43124"
	errSpyDial := errors.New("spy dial reached")
	hostPort := net.JoinHostPort(managedFixtureHost, publicPort)
	publicEndpoint := "127.0.0.1:54321"
	backendEndpoint := "127.0.0.1:" + backendPort
	routes := managedFixtureRoutes(hostPort, publicEndpoint, backendEndpoint)

	type spyDial struct {
		calls            int
		network, address string
	}
	newDial := func(routes []managedFixtureRoute, spy *spyDial) func(context.Context, string, string) (net.Conn, error) {
		return managedFixtureDialRoutesWith(routes, func(_ context.Context, network, address string) (net.Conn, error) {
			spy.calls++
			spy.network, spy.address = network, address
			return nil, errSpyDial
		})
	}
	refuse := func(t *testing.T, routes []managedFixtureRoute, network string, destinations ...string) {
		t.Helper()
		var spy spyDial
		dial := newDial(routes, &spy)
		for _, destination := range destinations {
			conn, err := dial(context.Background(), network, destination)
			if conn != nil || err == nil || errors.Is(err, errSpyDial) {
				t.Errorf("%s destination %q result = (%v, %v), want refusal before any dial", network, destination, conn, err)
			}
		}
		if spy.calls != 0 {
			t.Fatalf("underlying dial calls = %d, want zero", spy.calls)
		}
	}
	reach := func(t *testing.T, routes []managedFixtureRoute, destination, wantEndpoint string) {
		t.Helper()
		var spy spyDial
		conn, err := newDial(routes, &spy)(context.Background(), "tcp", destination)
		if conn != nil || !errors.Is(err, errSpyDial) {
			t.Fatalf("destination %q result = (%v, %v), want the underlying dial", destination, conn, err)
		}
		if spy.calls != 1 || spy.network != "tcp" || spy.address != wantEndpoint {
			t.Fatalf("destination %q reached calls=%d network=%q address=%q; want one tcp dial to %q",
				destination, spy.calls, spy.network, spy.address, wantEndpoint)
		}
	}

	t.Run("public-routes-to-owned-endpoint", func(t *testing.T) {
		reach(t, routes, hostPort, publicEndpoint)
	})

	t.Run("backend-routes-to-registered-endpoint", func(t *testing.T) {
		reach(t, routes, backendEndpoint, backendEndpoint)
	})

	t.Run("unregistered-addresses-no-dial", func(t *testing.T) {
		refuse(t, routes, "tcp",
			"127.0.0.1:43125",
			"127.0.0.1:"+publicPort,
			net.JoinHostPort(managedFixtureHost, backendPort),
			"127.0.0.2:"+backendPort,
			"localhost:"+backendPort,
			"[::1]:"+backendPort,
			publicEndpoint,
			"unknown.invalid:"+backendPort,
		)
	})

	t.Run("cross-fixture-no-dial", func(t *testing.T) {
		otherHostPort := net.JoinHostPort(managedFixtureHost, "43223")
		otherRoutes := managedFixtureRoutes(otherHostPort, "127.0.0.1:54421", "127.0.0.1:43224")
		refuse(t, routes, "tcp", otherHostPort, "127.0.0.1:43224", "127.0.0.1:54421")
		refuse(t, otherRoutes, "tcp", hostPort, backendEndpoint, publicEndpoint)
		reach(t, otherRoutes, otherHostPort, "127.0.0.1:54421")
		reach(t, otherRoutes, "127.0.0.1:43224", "127.0.0.1:43224")
	})

	t.Run("invalid-route-config-no-dial", func(t *testing.T) {
		invalid := map[string][]managedFixtureRoute{
			"empty":                    nil,
			"hostname-backend":         managedFixtureRoutes(hostPort, publicEndpoint, "localhost:"+backendPort),
			"noncanonical-port":        managedFixtureRoutes(hostPort, publicEndpoint, "127.0.0.1:043124"),
			"zero-port":                managedFixtureRoutes(hostPort, publicEndpoint, "127.0.0.1:0"),
			"nonloopback-backend":      managedFixtureRoutes(hostPort, publicEndpoint, "192.0.2.10:"+backendPort),
			"mapped-backend-spelling":  managedFixtureRoutes(hostPort, publicEndpoint, "[::ffff:127.0.0.1]:"+backendPort),
			"duplicate-destination":    {{destination: hostPort, endpoint: publicEndpoint}, {destination: hostPort, endpoint: backendEndpoint}},
			"nonloopback-public":       managedFixtureRoutes(hostPort, "192.0.2.10:54321", backendEndpoint),
			"nonfixture-public-domain": managedFixtureRoutes("other.tailnet.ts.net:"+publicPort, publicEndpoint, backendEndpoint),
		}
		for name, bad := range invalid {
			t.Run(name, func(t *testing.T) {
				refuse(t, bad, "tcp", hostPort, backendEndpoint, publicEndpoint)
			})
		}
		conn, err := managedFixtureDialRoutesWith(routes, nil)(context.Background(), "tcp", hostPort)
		if conn != nil || err == nil {
			t.Fatalf("nil underlying dialer result = (%v, %v), want refusal", conn, err)
		}
	})

	t.Run("non-tcp-network-no-dial", func(t *testing.T) {
		refuse(t, routes, "udp", hostPort, backendEndpoint)
		refuse(t, routes, "unix", hostPort, backendEndpoint)
	})
}
