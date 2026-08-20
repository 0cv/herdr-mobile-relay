// Package gatewaywire defines the wire contract shared by the blind WSS
// gateway, the computer relay that registers with it, and the phone app that
// connects through it.
//
// The gateway copies opaque frames. It never learns the relay key, never sees
// plaintext, SDP, or push subscriptions, and holds no long-lived secret of its
// own. Phone connections are authenticated by a challenge-response HMAC that
// the gateway forwards to the relay; the relay is the only party that can
// verify it, because only the relay and the paired phone can derive the
// rendezvous key from the relay key.
//
// Two identifiers are derived one-way from the relay key, so the QR payload is
// unchanged and the gateway learns nothing about the relay key:
//
//	relay_id       = base64url(HKDF-SHA256(relay_key, salt, "herdr-gw-id", 16))
//	rendezvous_key = HKDF-SHA256(relay_key, salt, "herdr-gw-auth", 32)
package gatewaywire

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// Proto is the gateway protocol version carried in every hello message.
const Proto = 1

// FrameVersion prefixes every multiplexed frame on the relay link.
const FrameVersion = 1

// Multiplex opcodes. Only the relay link is multiplexed; a phone connection
// carries bare encrypted frames because it is a single logical connection.
const (
	// OpData carries one verbatim Herdr E2EE frame (raw ciphertext).
	OpData byte = 0
	// OpOpen announces a new phone connection, gateway to relay only. The
	// payload is a JSON OpenPayload holding the challenge the phone answered.
	OpOpen byte = 1
	// OpClose ends one logical connection in either direction. The payload is
	// an optional short UTF-8 reason.
	OpClose byte = 2
	// OpPing and OpPong keep intermediaries from reaping an idle relay link.
	OpPing byte = 3
	OpPong byte = 4
	// OpNotice carries a gateway control message to the relay, currently
	// quota warnings. The payload is a JSON NoticePayload. Notices are
	// advisory: the relay surfaces them, the gateway never depends on them.
	OpNotice byte = 5
)

// HeaderSize is the fixed multiplex header: version, opcode, connection id.
const HeaderSize = 1 + 1 + 4

// MaxFramePayload bounds one multiplexed payload. It is far below the 21 MiB
// logical message cap because large logical messages travel as several
// encrypted frames on every transport that uses this protocol.
const MaxFramePayload = 1 << 20

// MaxCloseReason bounds the UTF-8 reason attached to OpClose.
const MaxCloseReason = 120

// NonceBytes is the length of the gateway challenge.
const NonceBytes = 32

// MaxHelloBytes bounds a JSON hello message.
const MaxHelloBytes = 4096

// RelayIDBytes is the raw length of a derived relay id before base64url.
const RelayIDBytes = 16

// RelayIDLength is the encoded relay id length, used for cheap validation.
const RelayIDLength = 22

const (
	hkdfSalt         = "herdr-gateway-v1"
	relayIDInfo      = "herdr-gw-id"
	rendezvousInfo   = "herdr-gw-auth"
	connectProofTag  = "herdr-gw-connect\x00"
	rendezvousKeyLen = 32
)

// ErrShortFrame reports a multiplex frame smaller than the fixed header.
var ErrShortFrame = errors.New("gateway frame is shorter than its header")

// DeriveRelayID returns the public rendezvous identifier for a relay key.
func DeriveRelayID(relayKey string) (string, error) {
	id, err := hkdf.Key(sha256.New, []byte(relayKey), []byte(hkdfSalt), relayIDInfo, RelayIDBytes)
	if err != nil {
		return "", fmt.Errorf("derive relay id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(id), nil
}

// DeriveRendezvousKey returns the secret both the relay and the paired phone
// use to authenticate a gateway connection. The gateway never receives it.
func DeriveRendezvousKey(relayKey string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, []byte(relayKey), []byte(hkdfSalt), rendezvousInfo, rendezvousKeyLen)
	if err != nil {
		return nil, fmt.Errorf("derive rendezvous key: %w", err)
	}
	return key, nil
}

// ConnectProof answers a gateway challenge. The relay id is bound into the tag
// so a proof captured for one relay cannot be replayed against another.
func ConnectProof(rendezvousKey []byte, relayID string, nonce []byte) []byte {
	mac := hmac.New(sha256.New, rendezvousKey)
	_, _ = mac.Write([]byte(connectProofTag))
	_, _ = mac.Write([]byte(relayID))
	_, _ = mac.Write(nonce)
	return mac.Sum(nil)
}

// VerifyConnectProof checks a phone proof in constant time.
func VerifyConnectProof(rendezvousKey []byte, relayID string, nonce, proof []byte) bool {
	if len(nonce) != NonceBytes || len(proof) != sha256.Size {
		return false
	}
	return hmac.Equal(proof, ConnectProof(rendezvousKey, relayID, nonce))
}

// ValidRelayID reports whether an identifier is shaped like a derived relay id.
func ValidRelayID(relayID string) bool {
	if len(relayID) != RelayIDLength {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(relayID)
	return err == nil && len(decoded) == RelayIDBytes
}

// AppendFrame appends one multiplexed frame to dst and returns the new slice.
// Callers reuse dst to keep the copy path allocation-free.
func AppendFrame(dst []byte, op byte, connID uint32, payload []byte) []byte {
	dst = append(dst, FrameVersion, op)
	dst = binary.BigEndian.AppendUint32(dst, connID)
	return append(dst, payload...)
}

// EncodeFrame builds one multiplexed frame.
func EncodeFrame(op byte, connID uint32, payload []byte) []byte {
	return AppendFrame(make([]byte, 0, HeaderSize+len(payload)), op, connID, payload)
}

// DecodeFrame splits one multiplexed frame. The returned payload aliases frame.
func DecodeFrame(frame []byte) (op byte, connID uint32, payload []byte, err error) {
	if len(frame) < HeaderSize {
		return 0, 0, nil, ErrShortFrame
	}
	if frame[0] != FrameVersion {
		return 0, 0, nil, fmt.Errorf("unsupported gateway frame version %d", frame[0])
	}
	op = frame[1]
	if op > OpNotice {
		return 0, 0, nil, fmt.Errorf("unknown gateway opcode %d", op)
	}
	connID = binary.BigEndian.Uint32(frame[2:HeaderSize])
	payload = frame[HeaderSize:]
	if len(payload) > MaxFramePayload {
		return 0, 0, nil, fmt.Errorf("gateway frame payload %d exceeds %d", len(payload), MaxFramePayload)
	}
	return op, connID, payload, nil
}

// ServerHello is the first message the gateway sends on any connection. The
// nonce is the challenge a phone answers; a relay ignores it.
//
// StunPort advertises the gateway's own STUN listener so both peers can learn
// their mapped address without router configuration. Only the port travels: each
// peer combines it with the gateway host it already dialed, so a gateway can
// never redirect address discovery to a third party. 0 means disabled.
type ServerHello struct {
	Type     string `json:"type"`
	Proto    int    `json:"proto"`
	Nonce    string `json:"nonce"`
	StunPort int    `json:"stun_port,omitempty"`
	Version  string `json:"version,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// RegisterHello claims a relay id for the multiplexed relay link.
type RegisterHello struct {
	Type    string `json:"type"`
	Proto   int    `json:"proto"`
	RelayID string `json:"relay_id"`
}

// ConnectHello asks to be paired with a registered relay.
type ConnectHello struct {
	Type    string `json:"type"`
	Proto   int    `json:"proto"`
	RelayID string `json:"relay_id"`
	Proof   string `json:"proof"`
}

// ReadyMessage tells a client that framing has started.
type ReadyMessage struct {
	Type  string `json:"type"`
	Proto int    `json:"proto"`
}

// ErrorMessage is the last message on a rejected connection.
type ErrorMessage struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OpenPayload is the JSON body of OpOpen. It hands the relay the challenge and
// the phone's answer so the relay — not the gateway — authenticates the phone.
type OpenPayload struct {
	Nonce string `json:"nonce"`
	Proof string `json:"proof"`
}

// NoticePayload is the JSON body of OpNotice.
type NoticePayload struct {
	Kind         string `json:"kind"`
	Message      string `json:"message"`
	RelayedBytes uint64 `json:"relayed_bytes,omitempty"`
	QuotaBytes   uint64 `json:"quota_bytes,omitempty"`
}

// Message type names used in the JSON hello exchange.
const (
	TypeServerHello = "gateway_hello"
	TypeRegister    = "register"
	TypeConnect     = "connect"
	TypeReady       = "ready"
	TypeError       = "error"
)

// Notice kinds.
const (
	NoticeQuotaWarning  = "quota_warning"
	NoticeQuotaExceeded = "quota_exceeded"
)

// Error codes returned in ErrorMessage.
const (
	CodeBadHello      = "bad_hello"
	CodeUnknownRelay  = "unknown_relay"
	CodeRateLimited   = "rate_limited"
	CodeTooManyClient = "too_many_clients"
	// CodeAtCapacity refuses a connection because the gateway as a whole is
	// full, not because the relay it names is. A shared public instance needs
	// ceilings on total relays and total phones that no per-relay cap can
	// express; a client seeing this should try again later or use another
	// gateway.
	CodeAtCapacity    = "at_capacity"
	CodeQuotaExceeded = "quota_exceeded"
	CodeRelayBusy     = "relay_busy"
	CodeInternal      = "internal"
)
