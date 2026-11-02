package gateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"

	"github.com/pion/stun/v4"
)

// assertPionBindingSuccess checks the actual datagram independently of the
// gateway's STUN decoder. Pion Decode accepts trailing bytes, so exact wire
// bounds and the attribute family/length remain explicit assertions.
func assertPionBindingSuccess(t *testing.T, wire []byte, txID [12]byte, want netip.AddrPort, requestSize, responseSize int, software bool) {
	t.Helper()
	if len(wire) != responseSize || len(wire) < 28 {
		t.Fatalf("response length = %d, want %d", len(wire), responseSize)
	}
	budget := 2 * requestSize
	if budget < 44 {
		budget = 44
	}
	if len(wire) > 64 || len(wire) > budget || (software && len(wire) > 2*requestSize) {
		t.Fatalf("%d-byte response exceeds the maximum or %d-byte request budget %d", len(wire), requestSize, budget)
	}
	if typ := binary.BigEndian.Uint16(wire[0:2]); typ != 0x0101 {
		t.Fatalf("wire type = %#04x, want 0x0101", typ)
	}
	if length := int(binary.BigEndian.Uint16(wire[2:4])); length != responseSize-20 || length+20 != len(wire) {
		t.Fatalf("header body length = %d for %d bytes", length, len(wire))
	}
	if cookie := binary.BigEndian.Uint32(wire[4:8]); cookie != 0x2112a442 {
		t.Fatalf("cookie = %#08x, want 0x2112a442", cookie)
	}

	message := stun.Message{Raw: wire}
	if err := message.Decode(); err != nil {
		t.Fatalf("Pion Decode: %v", err)
	}
	if message.Type != stun.BindingSuccess || message.TransactionID != txID {
		t.Fatalf("Pion type/transaction = %v/%x, want %v/%x", message.Type, message.TransactionID, stun.BindingSuccess, txID)
	}
	attrCount := 1
	if software {
		attrCount++
	}
	if len(message.Attributes) != attrCount {
		t.Fatalf("attribute count = %d, want %d", len(message.Attributes), attrCount)
	}
	family, addressLength := byte(1), 8
	if want.Addr().Is6() {
		family, addressLength = 2, 20
	}
	mappedAttr := message.Attributes[0]
	if mappedAttr.Type != stun.AttrType(0x0020) || int(mappedAttr.Length) != addressLength ||
		binary.BigEndian.Uint16(wire[20:22]) != 0x0020 || int(binary.BigEndian.Uint16(wire[22:24])) != addressLength {
		t.Fatalf("first attribute = %v length %d, want XOR-MAPPED-ADDRESS length %d", mappedAttr.Type, mappedAttr.Length, addressLength)
	}
	if wire[24] != 0 || wire[25] != family {
		t.Fatalf("reserved/family bytes = %02x/%02x, want 00/%02x", wire[24], wire[25], family)
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(&message); err != nil {
		t.Fatalf("Pion XOR-MAPPED-ADDRESS: %v", err)
	}
	address, ok := netip.AddrFromSlice(mapped.IP)
	if !ok || len(mapped.IP) != addressLength-4 || address != want.Addr() || mapped.Port != int(want.Port()) {
		t.Fatalf("Pion mapped address = %v:%d (%d IP bytes), want %s", mapped.IP, mapped.Port, len(mapped.IP), want)
	}
	if software {
		attr := message.Attributes[1]
		offset := 24 + addressLength
		if attr.Type != stun.AttrType(0x8022) || attr.Length != 13 || string(attr.Value) != "herdr-gateway" ||
			binary.BigEndian.Uint16(wire[offset:offset+2]) != 0x8022 || binary.BigEndian.Uint16(wire[offset+2:offset+4]) != 13 {
			t.Fatalf("second attribute = %v length %d value %q, want SOFTWARE herdr-gateway", attr.Type, attr.Length, attr.Value)
		}
		if !bytes.Equal(wire[offset+17:offset+20], []byte{0, 0, 0}) {
			t.Fatalf("SOFTWARE padding = %x, want 000000", wire[offset+17:offset+20])
		}
	}
}

func TestSTUNBindingSuccessPionOracle(t *testing.T) {
	ids := []struct {
		name string
		id   [12]byte
	}{
		{"first", [12]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x13, 0x57, 0x9b, 0xdf}},
		{"second", [12]byte{0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10, 0xa5, 0x5a, 0xc3, 0x3c}},
	}
	prefixes := []struct {
		name  string
		bytes []byte
	}{
		{"empty", nil},
		{"sentinel", []byte{0xd3, 0x9a, 0x00, 0xff, 0x51}},
	}
	cases := []struct {
		name                      string
		mapped                    netip.AddrPort
		requestSize, responseSize int
		software                  bool
	}{
		{"ipv4-bare", netip.MustParseAddrPort("192.0.2.1:1"), 20, 32, false},
		{"ipv4-24", netip.MustParseAddrPort("198.51.100.219:65535"), 24, 32, false},
		{"ipv4-28-software", netip.MustParseAddrPort("203.0.113.27:3478"), 28, 52, true},
		{"ipv6-bare", netip.MustParseAddrPort("[2001:db8:abcd:1234:fedc:ba98:7654:3210]:65535"), 20, 44, false},
		{"ipv6-28", netip.MustParseAddrPort("[2001:db8:1:2:3:4:5:6]:1"), 28, 44, false},
		{"ipv6-32-software", netip.MustParseAddrPort("[2001:db8:9876:5432:10fe:dcba:9876:1234]:49152"), 32, 64, true},
	}
	for _, tc := range cases {
		for _, id := range ids {
			for _, prefix := range prefixes {
				t.Run(tc.name+"/"+id.name+"/"+prefix.name, func(t *testing.T) {
					var dst []byte
					if len(prefix.bytes) != 0 {
						dst = make([]byte, len(prefix.bytes), 128)
						copy(dst, prefix.bytes)
					}
					out := appendBindingSuccess(dst, id.id[:], tc.mapped.Addr(), tc.mapped.Port(), tc.requestSize)
					if len(out) < len(prefix.bytes) || !bytes.Equal(out[:len(prefix.bytes)], prefix.bytes) {
						t.Fatal("appendBindingSuccess changed the destination prefix")
					}
					assertPionBindingSuccess(t, out[len(prefix.bytes):], id.id, tc.mapped, tc.requestSize, tc.responseSize, tc.software)
				})
			}
		}
	}
}

func isIPv6LoopbackUnavailable(err error) bool {
	return errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL)
}

func TestSTUNIPv6LoopbackUnavailable(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		syscall     string
		unavailable bool
	}{
		{"nil", nil, "", false},
		{"address-family", syscall.EAFNOSUPPORT, "socket", true},
		{"protocol", syscall.EPROTONOSUPPORT, "socket", true},
		{"address-unavailable", syscall.EADDRNOTAVAIL, "bind", true},
		{"unknown", errors.New("IPv6 loopback unavailable"), "socket", false},
		{"process-descriptors", syscall.EMFILE, "socket", false},
		{"system-descriptors", syscall.ENFILE, "socket", false},
		{"memory", syscall.ENOMEM, "socket", false},
		{"access", syscall.EACCES, "bind", false},
		{"permission", syscall.EPERM, "socket", false},
		{"address-in-use", syscall.EADDRINUSE, "bind", false},
		{"invalid", syscall.EINVAL, "bind", false},
	}
	for _, tc := range cases {
		variants := []struct {
			name string
			err  error
		}{{"direct", tc.err}}
		if tc.err != nil {
			variants = append(variants, struct {
				name string
				err  error
			}{"wrapped", &net.OpError{
				Op: "listen", Net: "udp6", Addr: &net.UDPAddr{IP: net.IPv6loopback},
				Err: &os.SyscallError{Syscall: tc.syscall, Err: tc.err},
			}})
		}
		for _, variant := range variants {
			t.Run(tc.name+"/"+variant.name, func(t *testing.T) {
				if got := isIPv6LoopbackUnavailable(variant.err); got != tc.unavailable {
					t.Fatalf("isIPv6LoopbackUnavailable(%v) = %v, want %v", variant.err, got, tc.unavailable)
				}
			})
		}
	}
}

func TestSTUNListenerPionOracle(t *testing.T) {
	first := [12]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x13, 0x57, 0x9b, 0xdf}
	second := [12]byte{0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10, 0xa5, 0x5a, 0xc3, 0x3c}
	cases := []struct {
		name                      string
		ip                        net.IP
		id                        [12]byte
		requestSoftware           string
		requestSize, responseSize int
		responseSoftware          bool
	}{
		{"ipv4-bare", net.IPv4(127, 0, 0, 1), first, "", 20, 32, false},
		{"ipv4-padded", net.IPv4(127, 0, 0, 1), second, "x", 28, 52, true},
		{"ipv6-bare", net.IPv6loopback, first, "", 20, 44, false},
		{"ipv6-padded", net.IPv6loopback, second, "hello", 32, 64, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ip.Equal(net.IPv6loopback) {
				probe, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
				if err != nil {
					if isIPv6LoopbackUnavailable(err) {
						t.Skipf("IPv6 loopback unavailable: %v", err)
					}
					t.Fatalf("IPv6 loopback probe failed: %v", err)
				}
				probe.Close()
			}
			_, conn := newSTUNHarness(t, tc.ip)
			setters := []stun.Setter{stun.BindingRequest, stun.NewTransactionIDSetter(tc.id)}
			if tc.requestSoftware != "" {
				setters = append(setters, stun.NewSoftware(tc.requestSoftware))
			}
			request, err := stun.Build(setters...)
			if err != nil {
				t.Fatalf("Pion build request: %v", err)
			}
			if len(request.Raw) != tc.requestSize {
				t.Fatalf("Pion request length = %d, want %d", len(request.Raw), tc.requestSize)
			}
			sendSTUN(t, conn, request.Raw)
			assertPionBindingSuccess(t, mustReadSTUN(t, conn), tc.id, localAddrPort(t, conn), tc.requestSize, tc.responseSize, tc.responseSoftware)
		})
	}
}
