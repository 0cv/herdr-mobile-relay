package portmap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestEncodePCPMap(t *testing.T) {
	var nonce [pcpNonceLen]byte
	copy(nonce[:], "0123456789ab")
	local := netip.MustParseAddr("192.168.1.34")

	req := encodePCPMap(nonce, local, 41234, 41234, netip.Addr{}, 7200)
	if len(req) != pcpRequestLen {
		t.Fatalf("request length = %d, want %d", len(req), pcpRequestLen)
	}
	if req[0] != pcpVersion {
		t.Errorf("version = %d, want %d", req[0], pcpVersion)
	}
	if req[1] != pcpOpMap {
		t.Errorf("opcode byte = %#x, want %#x (R bit clear)", req[1], pcpOpMap)
	}
	if lifetime := binary.BigEndian.Uint32(req[4:8]); lifetime != 7200 {
		t.Errorf("lifetime = %d, want 7200", lifetime)
	}

	wantClient := local.As16()
	if !bytes.Equal(req[8:24], wantClient[:]) {
		t.Errorf("client address = % x, want % x (ipv4-mapped)", req[8:24], wantClient)
	}
	if !bytes.Equal(req[24:36], nonce[:]) {
		t.Errorf("nonce = % x, want % x", req[24:36], nonce)
	}
	if req[36] != pcpProtoUDP {
		t.Errorf("protocol = %d, want %d", req[36], pcpProtoUDP)
	}
	if !bytes.Equal(req[37:40], []byte{0, 0, 0}) {
		t.Errorf("reserved bytes = % x, want zeroes", req[37:40])
	}
	if port := binary.BigEndian.Uint16(req[40:42]); port != 41234 {
		t.Errorf("internal port = %d, want 41234", port)
	}
	if port := binary.BigEndian.Uint16(req[42:44]); port != 41234 {
		t.Errorf("suggested external port = %d, want 41234", port)
	}
	if !bytes.Equal(req[44:60], make([]byte, 16)) {
		t.Errorf("suggested external address = % x, want zeroes", req[44:60])
	}
}

func TestEncodePCPMapDelete(t *testing.T) {
	var nonce [pcpNonceLen]byte
	req := encodePCPMap(nonce, netip.MustParseAddr("10.0.0.2"), 41234, 0, netip.Addr{}, 0)
	if lifetime := binary.BigEndian.Uint32(req[4:8]); lifetime != 0 {
		t.Errorf("delete lifetime = %d, want 0", lifetime)
	}
	if port := binary.BigEndian.Uint16(req[42:44]); port != 0 {
		t.Errorf("delete suggested external port = %d, want 0", port)
	}
}

func pcpMapResponse(code uint8, nonce [pcpNonceLen]byte, internal, external uint16, lifetime uint32, externalIP netip.Addr) []byte {
	resp := make([]byte, pcpRequestLen)
	resp[0] = pcpVersion
	resp[1] = pcpOpMap | pcpResponseBit
	resp[3] = code
	binary.BigEndian.PutUint32(resp[4:8], lifetime)
	copy(resp[24:36], nonce[:])
	resp[36] = pcpProtoUDP
	binary.BigEndian.PutUint16(resp[40:42], internal)
	binary.BigEndian.PutUint16(resp[42:44], external)
	copy(resp[44:60], addr16(externalIP))
	return resp
}

func TestParsePCPMapResponse(t *testing.T) {
	var nonce [pcpNonceLen]byte
	copy(nonce[:], "nonce-123456")
	resp := pcpMapResponse(pcpSuccess, nonce, 41234, 50000, 1800, netip.MustParseAddr("198.51.100.9"))

	got, err := parsePCPMapResponse(resp, nonce)
	if err != nil {
		t.Fatalf("parsePCPMapResponse: %v", err)
	}
	if got.InternalPort != 41234 {
		t.Errorf("internal port = %d, want 41234", got.InternalPort)
	}
	if got.External != netip.MustParseAddrPort("198.51.100.9:50000") {
		t.Errorf("external = %s, want 198.51.100.9:50000", got.External)
	}
	if got.Lifetime != 30*time.Minute {
		t.Errorf("lifetime = %s, want 30m", got.Lifetime)
	}
}

func TestParsePCPMapResponseErrors(t *testing.T) {
	var nonce [pcpNonceLen]byte
	copy(nonce[:], "nonce-123456")
	valid := pcpMapResponse(pcpSuccess, nonce, 41234, 50000, 1800, netip.MustParseAddr("198.51.100.9"))

	t.Run("result code", func(t *testing.T) {
		resp := pcpMapResponse(pcpNoResources, nonce, 41234, 0, 0, netip.Addr{})

		_, err := parsePCPMapResponse(resp, nonce)
		var pcpErr *pcpError
		if !errors.As(err, &pcpErr) {
			t.Fatalf("error = %v, want *pcpError", err)
		}
		if pcpErr.Code != pcpNoResources {
			t.Fatalf("code = %d, want %d", pcpErr.Code, pcpNoResources)
		}
		if pcpErr.Error() == "" {
			t.Fatal("empty error text")
		}
	})

	t.Run("nonce mismatch", func(t *testing.T) {
		var other [pcpNonceLen]byte
		copy(other[:], "different-no")
		if _, err := parsePCPMapResponse(valid, other); err == nil {
			t.Fatal("accepted a response with a foreign nonce")
		}
	})

	malformed := []struct {
		name string
		resp []byte
	}{
		{name: "empty", resp: nil},
		{name: "truncated", resp: valid[:pcpRequestLen-4]},
		{name: "unaligned", resp: append(bytes.Clone(valid), 0)},
		{name: "oversized", resp: make([]byte, pcpMaxRespBytes+4)},
		{name: "wrong version", resp: append([]byte{1}, valid[1:]...)},
		{name: "request bit", resp: append([]byte{valid[0], pcpOpMap}, valid[2:]...)},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePCPMapResponse(tc.resp, nonce); err == nil {
				t.Fatal("parsePCPMapResponse accepted a malformed response")
			}
		})
	}
}
