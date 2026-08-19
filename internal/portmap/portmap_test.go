package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakeNATPMP is an in-process NAT-PMP responder on loopback. It answers PCP
// requests the way a NAT-PMP-only router does, so the PCP attempt fails fast.
type fakeNATPMP struct {
	conn         *net.UDPConn
	port         uint16
	externalIP   [4]byte
	externalPort uint16
	lifetime     uint32

	mu       sync.Mutex
	external int
	maps     []fakeMapRequest
	events   chan fakeMapRequest
}

type fakeMapRequest struct {
	Internal uint16
	External uint16
	Lifetime uint32
}

func newFakeNATPMP(t *testing.T) *fakeNATPMP {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen fake nat-pmp: %v", err)
	}
	server := &fakeNATPMP{
		conn:         conn,
		port:         uint16(conn.LocalAddr().(*net.UDPAddr).Port),
		externalIP:   [4]byte{203, 0, 113, 11},
		externalPort: 50000,
		lifetime:     120,
		events:       make(chan fakeMapRequest, 16),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.serve()
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return server
}

func (f *fakeNATPMP) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		req := buf[:n]
		if len(req) < 2 {
			continue
		}

		if req[0] == pcpVersion {
			// RFC 6887 section 9: a NAT-PMP-only server rejects the version.
			resp := natpmpMapResponse(natpmpUnsupportedVersion, 0, 0, 0)
			f.conn.WriteToUDP(resp, from)
			continue
		}
		if req[0] != natpmpVersion {
			continue
		}

		switch req[1] {
		case natpmpOpExternal:
			f.mu.Lock()
			f.external++
			f.mu.Unlock()

			resp := make([]byte, natpmpExtRespLen)
			resp[1] = natpmpOpExternal | natpmpResponseBit
			copy(resp[8:12], f.externalIP[:])
			f.conn.WriteToUDP(resp, from)
		case natpmpOpMapUDP:
			if len(req) < natpmpMapReqLen {
				continue
			}
			record := fakeMapRequest{
				Internal: binary.BigEndian.Uint16(req[4:6]),
				External: binary.BigEndian.Uint16(req[6:8]),
				Lifetime: binary.BigEndian.Uint32(req[8:12]),
			}
			f.mu.Lock()
			f.maps = append(f.maps, record)
			f.mu.Unlock()
			select {
			case f.events <- record:
			default:
			}

			granted := f.lifetime
			assigned := f.externalPort
			if record.Lifetime == 0 {
				granted, assigned = 0, 0
			}
			f.conn.WriteToUDP(natpmpMapResponse(natpmpSuccess, record.Internal, assigned, granted), from)
		}
	}
}

func (f *fakeNATPMP) nextMap(t *testing.T) fakeMapRequest {
	t.Helper()
	select {
	case req := <-f.events:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a nat-pmp map request")
		return fakeMapRequest{}
	}
}

func (f *fakeNATPMP) mapCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.maps)
}

func (f *fakeNATPMP) mapsSnapshot() []fakeMapRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeMapRequest(nil), f.maps...)
}

// testClient points the mapper at the loopback responder and keeps UPnP out of
// the way so no test ever touches the real network.
func testClient(port uint16) *client {
	return &client{
		logger: testLogger(),
		gateways: func(context.Context) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		serverPort: port,
		allowUPnP:  false,
		ssdpAddr:   ssdpMulticastAddr,
		httpClient: newUPnPHTTPClient(),
	}
}

func TestMapUDPFallsBackFromPCPToNATPMP(t *testing.T) {
	server := newFakeNATPMP(t)
	c := testClient(server.port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mapping, err := c.mapUDP(ctx, 41234, 10*time.Minute)
	if err != nil {
		t.Fatalf("mapUDP: %v", err)
	}
	if mapping.Method != MethodNATPMP {
		t.Fatalf("method = %s, want %s", mapping.Method, MethodNATPMP)
	}
	if got, want := mapping.External, netip.MustParseAddrPort("203.0.113.11:50000"); got != want {
		t.Errorf("external = %s, want %s", got, want)
	}
	if mapping.Internal != 41234 {
		t.Errorf("internal = %d, want 41234", mapping.Internal)
	}
	if mapping.Lifetime != 120*time.Second {
		t.Errorf("lifetime = %s, want the granted 120s", mapping.Lifetime)
	}
	if mapping.ExpiresAt.IsZero() {
		t.Error("ExpiresAt not set")
	}

	requested := server.nextMap(t)
	if requested.Internal != 41234 || requested.Lifetime != 600 {
		t.Errorf("map request = %+v, want internal 41234 lifetime 600", requested)
	}

	if err := mapping.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	released := server.nextMap(t)
	if released.Internal != 41234 || released.External != 0 || released.Lifetime != 0 {
		t.Errorf("release request = %+v, want internal 41234 external 0 lifetime 0", released)
	}
}

func TestMapUDPRejectsPortZero(t *testing.T) {
	c := testClient(1)
	if _, err := c.mapUDP(context.Background(), 0, time.Minute); err == nil {
		t.Fatal("mapUDP accepted internal port 0")
	}
}

func TestMapUDPWithoutAnyResponder(t *testing.T) {
	// Nothing listens on this port: every protocol must fail, and quickly.
	c := testClient(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := c.mapUDP(ctx, 41234, time.Minute); !errors.Is(err, ErrNoMethod) {
		t.Fatalf("error = %v, want ErrNoMethod", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("mapUDP took %s, want a bounded attempt", elapsed)
	}
}

func TestClampLifetime(t *testing.T) {
	if got := clampLifetime(time.Second); got != minLifetime {
		t.Errorf("clampLifetime(1s) = %s, want %s", got, minLifetime)
	}
	if got := clampLifetime(48 * time.Hour); got != maxLifetime {
		t.Errorf("clampLifetime(48h) = %s, want %s", got, maxLifetime)
	}
	if got := clampLifetime(time.Hour); got != time.Hour {
		t.Errorf("clampLifetime(1h) = %s, want 1h", got)
	}
}

func TestMapperRenewsAtHalfLifetimeAndReleasesOnClose(t *testing.T) {
	server := newFakeNATPMP(t)

	mapper := NewMapper(testLogger())
	mapper.client = testClient(server.port)
	mapper.lifetime = 10 * time.Minute

	waits := make(chan time.Duration, 4)
	renew := make(chan time.Time)
	mapper.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return renew
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		mapper.Run(ctx, 41234)
	}()

	first := server.nextMap(t)
	if first.Internal != 41234 || first.Lifetime != 600 {
		t.Fatalf("first map request = %+v", first)
	}
	if wait := <-waits; wait != 60*time.Second {
		t.Fatalf("renewal wait = %s, want half of the granted 120s", wait)
	}

	mapping, ok := mapper.Current()
	if !ok {
		t.Fatal("Current reported no mapping after a successful map")
	}
	if mapping.Method != MethodNATPMP || mapping.External.Port() != 50000 {
		t.Fatalf("current mapping = %+v", mapping)
	}

	renew <- time.Now()
	second := server.nextMap(t)
	if second.Internal != 41234 || second.Lifetime != 600 {
		t.Fatalf("renewal map request = %+v", second)
	}
	if wait := <-waits; wait != 60*time.Second {
		t.Fatalf("second renewal wait = %s, want 60s", wait)
	}

	mapper.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Close")
	}

	if _, ok := mapper.Current(); ok {
		t.Error("Current still reports a mapping after Close")
	}
	if got := server.mapCount(); got != 3 {
		t.Fatalf("map requests = %d, want 2 mappings and 1 release", got)
	}
	release := server.mapsSnapshot()[2]
	if release.Internal != 41234 || release.External != 0 || release.Lifetime != 0 {
		t.Fatalf("release request = %+v, want internal 41234 external 0 lifetime 0", release)
	}

	// Close is idempotent and must not send a second release.
	mapper.Close()
	if got := server.mapCount(); got != 3 {
		t.Fatalf("map requests after second Close = %d, want 3", got)
	}
}

func TestMapperRunStopsWithContext(t *testing.T) {
	server := newFakeNATPMP(t)

	mapper := NewMapper(testLogger())
	mapper.client = testClient(server.port)
	// Receiving a renewal wait proves the mapping was recorded first.
	waits := make(chan time.Duration, 2)
	mapper.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return make(chan time.Time)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		mapper.Run(ctx, 41234)
	}()

	server.nextMap(t)
	<-waits
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored context cancellation")
	}

	// Close after the context died still releases the live mapping.
	mapper.Close()
	if got := server.mapCount(); got != 2 {
		t.Fatalf("map requests = %d, want the mapping plus its release", got)
	}
}

func TestMapperRunWithoutPortDoesNothing(t *testing.T) {
	mapper := NewMapper(testLogger())
	mapper.client = testClient(1)
	mapper.Run(context.Background(), 0)
	if _, ok := mapper.Current(); ok {
		t.Fatal("Run(0) produced a mapping")
	}
	mapper.Close()
}
