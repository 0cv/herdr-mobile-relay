package portmap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestEncodeNATPMPMap(t *testing.T) {
	got := encodeNATPMPMap(41234, 41234, 7200)
	want := []byte{
		0x00, 0x01, // version, opcode UDP map
		0x00, 0x00, // reserved
		0xa1, 0x12, // internal port 41234
		0xa1, 0x12, // suggested external port
		0x00, 0x00, 0x1c, 0x20, // lifetime 7200
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encodeNATPMPMap = % x, want % x", got, want)
	}
}

func TestEncodeNATPMPMapDelete(t *testing.T) {
	got := encodeNATPMPMap(41234, 0, 0)
	if len(got) != natpmpMapReqLen {
		t.Fatalf("request length = %d, want %d", len(got), natpmpMapReqLen)
	}
	if port := binary.BigEndian.Uint16(got[6:8]); port != 0 {
		t.Errorf("delete external port = %d, want 0", port)
	}
	if lifetime := binary.BigEndian.Uint32(got[8:12]); lifetime != 0 {
		t.Errorf("delete lifetime = %d, want 0", lifetime)
	}
}

func TestEncodeNATPMPExternal(t *testing.T) {
	if got := encodeNATPMPExternal(); !bytes.Equal(got, []byte{0x00, 0x00}) {
		t.Fatalf("encodeNATPMPExternal = % x", got)
	}
}

func natpmpMapResponse(result uint16, internal, external uint16, lifetime uint32) []byte {
	resp := make([]byte, natpmpMapRespLen)
	resp[0] = natpmpVersion
	resp[1] = natpmpOpMapUDP | natpmpResponseBit
	binary.BigEndian.PutUint16(resp[2:4], result)
	binary.BigEndian.PutUint32(resp[4:8], 1234)
	binary.BigEndian.PutUint16(resp[8:10], internal)
	binary.BigEndian.PutUint16(resp[10:12], external)
	binary.BigEndian.PutUint32(resp[12:16], lifetime)
	return resp
}

func TestParseNATPMPMap(t *testing.T) {
	got, err := parseNATPMPMap(natpmpMapResponse(natpmpSuccess, 41234, 50000, 3600))
	if err != nil {
		t.Fatalf("parseNATPMPMap: %v", err)
	}
	if got.InternalPort != 41234 || got.ExternalPort != 50000 {
		t.Errorf("ports = %d/%d, want 41234/50000", got.InternalPort, got.ExternalPort)
	}
	if got.Lifetime != time.Hour {
		t.Errorf("lifetime = %s, want 1h", got.Lifetime)
	}
}

func TestParseNATPMPMapResultCodes(t *testing.T) {
	for _, code := range []uint16{
		natpmpUnsupportedVersion,
		natpmpNotAuthorized,
		natpmpNetworkFailure,
		natpmpOutOfResources,
		natpmpUnsupportedOpcode,
	} {
		_, err := parseNATPMPMap(natpmpMapResponse(code, 41234, 0, 0))

		var mapErr *natpmpError
		if !errors.As(err, &mapErr) {
			t.Fatalf("result %d: error = %v, want *natpmpError", code, err)
		}
		if mapErr.Code != code {
			t.Errorf("result %d: reported code %d", code, mapErr.Code)
		}
		if mapErr.Error() == "" {
			t.Errorf("result %d: empty error text", code)
		}
	}
}

func TestParseNATPMPMapMalformed(t *testing.T) {
	valid := natpmpMapResponse(natpmpSuccess, 41234, 50000, 3600)

	tests := []struct {
		name string
		resp []byte
	}{
		{name: "empty", resp: nil},
		{name: "truncated header", resp: valid[:3]},
		{name: "truncated body", resp: valid[:natpmpMapRespLen-1]},
		{name: "oversized", resp: make([]byte, natpmpMaxRespBytes+1)},
		{name: "wrong version", resp: append([]byte{9}, valid[1:]...)},
		{name: "wrong opcode", resp: append([]byte{valid[0], 0x82}, valid[2:]...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseNATPMPMap(tc.resp); err == nil {
				t.Fatal("parseNATPMPMap accepted a malformed response")
			}
		})
	}
}

func TestParseNATPMPExternal(t *testing.T) {
	resp := make([]byte, natpmpExtRespLen)
	resp[1] = natpmpOpExternal | natpmpResponseBit
	copy(resp[8:12], []byte{203, 0, 113, 7})

	addr, err := parseNATPMPExternal(resp)
	if err != nil {
		t.Fatalf("parseNATPMPExternal: %v", err)
	}
	if addr != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("address = %s, want 203.0.113.7", addr)
	}

	if _, err := parseNATPMPExternal(resp[:7]); err == nil {
		t.Fatal("parseNATPMPExternal accepted a truncated response")
	}
}
