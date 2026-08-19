package portmap

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const (
	procNetRoute = "/proc/net/route"
	// rtfGateway is RTF_GATEWAY from <linux/route.h>.
	rtfGateway = 0x0002
	// maxRouteBytes bounds how much routing table we are willing to read.
	maxRouteBytes = 256 << 10
	maxRouteLines = 4096
)

// defaultGateways reports the default-gateway addresses of the host. An empty
// result is not fatal: UPnP discovery can still find an IGD over SSDP.
func defaultGateways(ctx context.Context) ([]netip.Addr, error) {
	switch runtime.GOOS {
	case "linux":
		file, err := os.Open(procNetRoute)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", procNetRoute, err)
		}
		defer file.Close()
		return parseProcNetRoute(file), nil
	case "darwin":
		out, err := exec.CommandContext(ctx, "route", "-n", "get", "default").Output()
		if err != nil {
			return nil, fmt.Errorf("route -n get default: %w", err)
		}
		return parseRouteGet(string(out)), nil
	}
	return nil, fmt.Errorf("gateway discovery unsupported on %s", runtime.GOOS)
}

// parseProcNetRoute extracts IPv4 default gateways from the Linux routing
// table. Addresses are little-endian hex words in that file.
func parseProcNetRoute(r io.Reader) []netip.Addr {
	scanner := bufio.NewScanner(io.LimitReader(r, maxRouteBytes))
	var (
		out  []netip.Addr
		seen = make(map[netip.Addr]struct{})
	)
	for lines := 0; lines < maxRouteLines && scanner.Scan(); lines++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&rtfGateway == 0 {
			continue
		}
		addr, ok := parseHexIPv4(fields[2])
		if !ok || addr.IsUnspecified() {
			continue
		}
		if _, dup := seen[addr]; dup {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	return out
}

func parseHexIPv4(field string) (netip.Addr, bool) {
	if len(field) != 8 {
		return netip.Addr{}, false
	}
	value, err := strconv.ParseUint(field, 16, 32)
	if err != nil {
		return netip.Addr{}, false
	}
	var octets [4]byte
	binary.LittleEndian.PutUint32(octets[:], uint32(value))
	return netip.AddrFrom4(octets), true
}

// parseRouteGet extracts the gateway line from `route -n get default` output.
func parseRouteGet(out string) []netip.Addr {
	if len(out) > maxRouteBytes {
		out = out[:maxRouteBytes]
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "gateway" {
			continue
		}
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || addr.IsUnspecified() {
			continue
		}
		return []netip.Addr{addr.Unmap()}
	}
	return nil
}
