package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// NAT-PMP (RFC 6886) wire constants.
const (
	natpmpVersion      = 0
	natpmpOpExternal   = 0
	natpmpOpMapUDP     = 1
	natpmpResponseBit  = 0x80
	natpmpMapReqLen    = 12
	natpmpMapRespLen   = 16
	natpmpExtReqLen    = 2
	natpmpExtRespLen   = 12
	natpmpMaxRespBytes = 64
)

// NAT-PMP result codes (RFC 6886 section 3.5).
const (
	natpmpSuccess            uint16 = 0
	natpmpUnsupportedVersion uint16 = 1
	natpmpNotAuthorized      uint16 = 2
	natpmpNetworkFailure     uint16 = 3
	natpmpOutOfResources     uint16 = 4
	natpmpUnsupportedOpcode  uint16 = 5
)

// natpmpError reports a non-zero NAT-PMP result code.
type natpmpError struct {
	Code uint16
}

func (e *natpmpError) Error() string {
	return fmt.Sprintf("nat-pmp result %d (%s)", e.Code, natpmpResultText(e.Code))
}

func natpmpResultText(code uint16) string {
	switch code {
	case natpmpSuccess:
		return "success"
	case natpmpUnsupportedVersion:
		return "unsupported version"
	case natpmpNotAuthorized:
		return "not authorized or refused"
	case natpmpNetworkFailure:
		return "network failure"
	case natpmpOutOfResources:
		return "out of resources"
	case natpmpUnsupportedOpcode:
		return "unsupported opcode"
	}
	return "unknown"
}

// encodeNATPMPMap builds a UDP mapping request. A lifetime of zero and an
// external port of zero delete the mapping (RFC 6886 section 3.4).
func encodeNATPMPMap(internalPort, externalPort uint16, lifetime uint32) []byte {
	req := make([]byte, natpmpMapReqLen)
	req[0] = natpmpVersion
	req[1] = natpmpOpMapUDP
	binary.BigEndian.PutUint16(req[4:6], internalPort)
	binary.BigEndian.PutUint16(req[6:8], externalPort)
	binary.BigEndian.PutUint32(req[8:12], lifetime)
	return req
}

// encodeNATPMPExternal builds an external-address request.
func encodeNATPMPExternal() []byte {
	return []byte{natpmpVersion, natpmpOpExternal}
}

// checkNATPMPHeader validates the shared response prefix.
func checkNATPMPHeader(resp []byte, opcode byte, want int) (uint16, error) {
	if len(resp) < want {
		return 0, fmt.Errorf("nat-pmp response truncated: %d bytes, want %d", len(resp), want)
	}
	if len(resp) > natpmpMaxRespBytes {
		return 0, fmt.Errorf("nat-pmp response oversized: %d bytes", len(resp))
	}
	if resp[0] != natpmpVersion {
		return 0, fmt.Errorf("nat-pmp response version %d unsupported", resp[0])
	}
	if resp[1] != opcode|natpmpResponseBit {
		return 0, fmt.Errorf("nat-pmp response opcode %#x unexpected", resp[1])
	}
	code := binary.BigEndian.Uint16(resp[2:4])
	if code != natpmpSuccess {
		return code, &natpmpError{Code: code}
	}
	return code, nil
}

// natpmpMapResult is a parsed successful mapping response.
type natpmpMapResult struct {
	InternalPort uint16
	ExternalPort uint16
	Lifetime     time.Duration
}

func parseNATPMPMap(resp []byte) (natpmpMapResult, error) {
	if _, err := checkNATPMPHeader(resp, natpmpOpMapUDP, natpmpMapRespLen); err != nil {
		return natpmpMapResult{}, err
	}
	return natpmpMapResult{
		InternalPort: binary.BigEndian.Uint16(resp[8:10]),
		ExternalPort: binary.BigEndian.Uint16(resp[10:12]),
		Lifetime:     time.Duration(binary.BigEndian.Uint32(resp[12:16])) * time.Second,
	}, nil
}

func parseNATPMPExternal(resp []byte) (netip.Addr, error) {
	if _, err := checkNATPMPHeader(resp, natpmpOpExternal, natpmpExtRespLen); err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4([4]byte(resp[8:12])), nil
}

func (c *client) mapNATPMP(ctx context.Context, gateway netip.Addr, internalPort uint16, lifetime time.Duration) (*Mapping, error) {
	seconds := uint32(lifetime / time.Second)
	resp, err := c.exchange(ctx, gateway, encodeNATPMPMap(internalPort, internalPort, seconds), natpmpMaxRespBytes)
	if err != nil {
		return nil, err
	}
	result, err := parseNATPMPMap(resp)
	if err != nil {
		return nil, err
	}
	if result.ExternalPort == 0 {
		return nil, errors.New("nat-pmp granted external port 0")
	}
	granted := result.Lifetime
	if granted <= 0 {
		granted = lifetime
	}

	external := netip.IPv4Unspecified()
	if addr, err := c.natpmpExternalAddr(ctx, gateway); err != nil {
		c.logger.Debug("nat-pmp external address unavailable", "error", err)
	} else {
		external = addr
	}

	return &Mapping{
		External:  netip.AddrPortFrom(external, result.ExternalPort),
		Internal:  internalPort,
		Method:    MethodNATPMP,
		Lifetime:  granted,
		ExpiresAt: time.Now().Add(granted),
		owner:     c,
		gateway:   gateway,
	}, nil
}

func (c *client) natpmpExternalAddr(ctx context.Context, gateway netip.Addr) (netip.Addr, error) {
	resp, err := c.exchange(ctx, gateway, encodeNATPMPExternal(), natpmpMaxRespBytes)
	if err != nil {
		return netip.Addr{}, err
	}
	return parseNATPMPExternal(resp)
}

func (c *client) releaseNATPMP(ctx context.Context, m *Mapping) error {
	resp, err := c.exchange(ctx, m.gateway, encodeNATPMPMap(m.Internal, 0, 0), natpmpMaxRespBytes)
	if err != nil {
		return err
	}
	_, err = parseNATPMPMap(resp)
	return err
}

// UDP retry schedule. RFC 6886 asks for exponential backoff starting at 250 ms;
// we keep three tries so the whole exchange stays inside stepTimeout.
const (
	udpInitialTimeout = 250 * time.Millisecond
	udpMaxAttempts    = 3
)

// exchange sends one request to the router's PCP/NAT-PMP port and returns the
// first response. The buffer is one byte larger than maxResponse so an
// oversized datagram is detected instead of silently truncated.
func (c *client) exchange(ctx context.Context, gateway netip.Addr, request []byte, maxResponse int) ([]byte, error) {
	if !gateway.IsValid() {
		return nil, errors.New("invalid gateway address")
	}
	ctx, cancel := context.WithTimeout(ctx, stepTimeout)
	defer cancel()

	conn, err := net.Dial("udp", netip.AddrPortFrom(gateway, c.serverPort).String())
	if err != nil {
		return nil, fmt.Errorf("dial router: %w", err)
	}
	defer conn.Close()

	hardDeadline, ok := ctx.Deadline()
	if !ok {
		hardDeadline = time.Now().Add(stepTimeout)
	}
	buf := make([]byte, maxResponse+1)
	timeout := udpInitialTimeout

	for attempt := 0; attempt < udpMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := conn.Write(request); err != nil {
			return nil, fmt.Errorf("write request: %w", err)
		}

		deadline := time.Now().Add(timeout)
		if deadline.After(hardDeadline) {
			deadline = hardDeadline
		}
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		n, err := conn.Read(buf)
		if err == nil {
			if n > maxResponse {
				return nil, fmt.Errorf("router response oversized: more than %d bytes", maxResponse)
			}
			return buf[:n], nil
		}

		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			timeout *= 2
			continue
		}
		return nil, fmt.Errorf("read response: %w", err)
	}
	return nil, fmt.Errorf("no response from %s after %d attempts", gateway, udpMaxAttempts)
}
