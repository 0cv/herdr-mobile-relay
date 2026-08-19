package portmap

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// PCP (RFC 6887) wire constants.
const (
	pcpVersion       = 2
	pcpOpMap         = 1
	pcpResponseBit   = 0x80
	pcpNonceLen      = 12
	pcpProtoUDP      = 17
	pcpHeaderLen     = 24
	pcpMapDataLen    = 36
	pcpRequestLen    = pcpHeaderLen + pcpMapDataLen
	pcpMaxRespBytes  = 1100
	pcpMinRespBytes  = pcpHeaderLen + pcpMapDataLen
	pcpAddrLen       = 16
	pcpLengthQuantum = 4
)

// PCP result codes (RFC 6887 section 7.4).
const (
	pcpSuccess               uint8 = 0
	pcpUnsuppVersion         uint8 = 1
	pcpNotAuthorized         uint8 = 2
	pcpMalformedRequest      uint8 = 3
	pcpUnsuppOpcode          uint8 = 4
	pcpUnsuppOption          uint8 = 5
	pcpMalformedOption       uint8 = 6
	pcpNetworkFailure        uint8 = 7
	pcpNoResources           uint8 = 8
	pcpUnsuppProtocol        uint8 = 9
	pcpUserExQuota           uint8 = 10
	pcpCannotProvideExternal uint8 = 11
	pcpAddressMismatch       uint8 = 12
	pcpExcessiveRemotePeers  uint8 = 13
)

// pcpError reports a non-success PCP result code.
type pcpError struct {
	Code uint8
}

func (e *pcpError) Error() string {
	return fmt.Sprintf("pcp result %d (%s)", e.Code, pcpResultText(e.Code))
}

func pcpResultText(code uint8) string {
	switch code {
	case pcpSuccess:
		return "success"
	case pcpUnsuppVersion:
		return "unsupported version"
	case pcpNotAuthorized:
		return "not authorized"
	case pcpMalformedRequest:
		return "malformed request"
	case pcpUnsuppOpcode:
		return "unsupported opcode"
	case pcpUnsuppOption:
		return "unsupported option"
	case pcpMalformedOption:
		return "malformed option"
	case pcpNetworkFailure:
		return "network failure"
	case pcpNoResources:
		return "no resources"
	case pcpUnsuppProtocol:
		return "unsupported protocol"
	case pcpUserExQuota:
		return "user exceeded quota"
	case pcpCannotProvideExternal:
		return "cannot provide external address"
	case pcpAddressMismatch:
		return "address mismatch"
	case pcpExcessiveRemotePeers:
		return "excessive remote peers"
	}
	return "unknown"
}

// encodePCPMap builds a MAP request. suggestedExternal may be the zero Addr,
// which asks the router to choose.
func encodePCPMap(nonce [pcpNonceLen]byte, local netip.Addr, internalPort, suggestedPort uint16, suggestedExternal netip.Addr, lifetime uint32) []byte {
	req := make([]byte, pcpRequestLen)
	req[0] = pcpVersion
	req[1] = pcpOpMap // R bit clear: request
	binary.BigEndian.PutUint32(req[4:8], lifetime)
	copy(req[8:24], addr16(local))

	copy(req[24:36], nonce[:])
	req[36] = pcpProtoUDP
	binary.BigEndian.PutUint16(req[40:42], internalPort)
	binary.BigEndian.PutUint16(req[42:44], suggestedPort)
	copy(req[44:60], addr16(suggestedExternal))
	return req
}

// addr16 renders an address in the 16-byte form PCP uses; IPv4 travels as an
// IPv4-mapped IPv6 address. An invalid address becomes all zeroes.
func addr16(addr netip.Addr) []byte {
	if !addr.IsValid() {
		return make([]byte, pcpAddrLen)
	}
	buf := addr.As16()
	return buf[:]
}

// pcpMapResult is a parsed successful MAP response.
type pcpMapResult struct {
	InternalPort uint16
	External     netip.AddrPort
	Lifetime     time.Duration
}

func parsePCPMapResponse(resp []byte, nonce [pcpNonceLen]byte) (pcpMapResult, error) {
	if len(resp) < pcpMinRespBytes {
		return pcpMapResult{}, fmt.Errorf("pcp response truncated: %d bytes, want %d", len(resp), pcpMinRespBytes)
	}
	if len(resp) > pcpMaxRespBytes {
		return pcpMapResult{}, fmt.Errorf("pcp response oversized: %d bytes", len(resp))
	}
	if len(resp)%pcpLengthQuantum != 0 {
		return pcpMapResult{}, fmt.Errorf("pcp response length %d is not a multiple of 4", len(resp))
	}
	if resp[0] != pcpVersion {
		return pcpMapResult{}, fmt.Errorf("pcp response version %d unsupported", resp[0])
	}
	if resp[1] != pcpOpMap|pcpResponseBit {
		return pcpMapResult{}, fmt.Errorf("pcp response opcode %#x unexpected", resp[1])
	}
	if code := resp[3]; code != pcpSuccess {
		return pcpMapResult{}, &pcpError{Code: code}
	}
	if subtle.ConstantTimeCompare(resp[24:36], nonce[:]) != 1 {
		return pcpMapResult{}, errors.New("pcp response nonce mismatch")
	}
	if resp[36] != pcpProtoUDP {
		return pcpMapResult{}, fmt.Errorf("pcp response protocol %d unexpected", resp[36])
	}

	external, ok := netip.AddrFromSlice(resp[44:60])
	if !ok {
		return pcpMapResult{}, errors.New("pcp response external address malformed")
	}
	return pcpMapResult{
		InternalPort: binary.BigEndian.Uint16(resp[40:42]),
		External:     netip.AddrPortFrom(external.Unmap(), binary.BigEndian.Uint16(resp[42:44])),
		Lifetime:     time.Duration(binary.BigEndian.Uint32(resp[4:8])) * time.Second,
	}, nil
}

func (c *client) mapPCP(ctx context.Context, gateway netip.Addr, internalPort uint16, lifetime time.Duration) (*Mapping, error) {
	local, err := c.localAddrFor(gateway)
	if err != nil {
		return nil, err
	}

	var nonce [pcpNonceLen]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generate pcp nonce: %w", err)
	}

	seconds := uint32(lifetime / time.Second)
	req := encodePCPMap(nonce, local, internalPort, internalPort, netip.Addr{}, seconds)
	resp, err := c.exchange(ctx, gateway, req, pcpMaxRespBytes)
	if err != nil {
		return nil, err
	}
	result, err := parsePCPMapResponse(resp, nonce)
	if err != nil {
		return nil, err
	}
	if result.External.Port() == 0 {
		return nil, errors.New("pcp granted external port 0")
	}
	granted := result.Lifetime
	if granted <= 0 {
		granted = lifetime
	}

	return &Mapping{
		External:  result.External,
		Internal:  internalPort,
		Method:    MethodPCP,
		Lifetime:  granted,
		ExpiresAt: time.Now().Add(granted),
		owner:     c,
		gateway:   gateway,
		local:     local,
		nonce:     nonce,
	}, nil
}

func (c *client) releasePCP(ctx context.Context, m *Mapping) error {
	req := encodePCPMap(m.nonce, m.local, m.Internal, 0, netip.Addr{}, 0)
	resp, err := c.exchange(ctx, m.gateway, req, pcpMaxRespBytes)
	if err != nil {
		return err
	}
	_, err = parsePCPMapResponse(resp, m.nonce)
	return err
}
