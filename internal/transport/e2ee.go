package transport

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	relayprotocol "github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const (
	e2eeSubprotocol             = relayprotocol.EncryptedWebSocketSubprotocol
	e2eeVersion                 = 2
	e2eeHandshakeTimeout        = 10 * time.Second
	e2eeNonceBytes              = 32
	e2eePublicKeyBytes          = 65
	e2eeSecretBytes             = 32
	maxE2EESequence      uint64 = 1<<53 - 1

	e2eeClientDirection = "c2s"
	e2eeServerDirection = "s2c"
)

// ErrDeviceAuthRejected marks a handshake this relay will keep refusing: the
// credential is unknown, revoked, superseded, or the invitation is spent.
// Reconnecting with the same material cannot help, so the phone is told to
// stop and pair again.
var ErrDeviceAuthRejected = errors.New("device authentication rejected")

type FrameCodec int

const (
	CodecJSON FrameCodec = iota
	CodecBinary
)

const (
	binaryFrameVersion    = e2eeVersion
	binaryFrameKindData   = 0
	binaryFrameHeaderSize = 1 + 1 + 8
)

func (c FrameCodec) encodeFrame(sequence uint64, ciphertext []byte) ([]byte, error) {
	if c == CodecBinary {
		frame := make([]byte, binaryFrameHeaderSize+len(ciphertext))
		frame[0] = binaryFrameVersion
		frame[1] = binaryFrameKindData
		binary.BigEndian.PutUint64(frame[2:], sequence)
		copy(frame[binaryFrameHeaderSize:], ciphertext)
		return frame, nil
	}
	return json.Marshal(e2eeFrame{
		Type:       "e2ee",
		Version:    e2eeVersion,
		Sequence:   sequence,
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	})
}

func (c FrameCodec) decodeFrame(rawFrame []byte) (uint64, []byte, error) {
	if c == CodecBinary {
		if len(rawFrame) < binaryFrameHeaderSize {
			return 0, nil, errors.New("invalid encrypted frame")
		}
		if rawFrame[0] != binaryFrameVersion || rawFrame[1] != binaryFrameKindData {
			return 0, nil, errors.New("unsupported encrypted frame")
		}
		return binary.BigEndian.Uint64(rawFrame[2:]), rawFrame[binaryFrameHeaderSize:], nil
	}
	var frame e2eeFrame
	if err := json.Unmarshal(rawFrame, &frame); err != nil {
		return 0, nil, errors.New("invalid encrypted frame")
	}
	if frame.Type != "e2ee" || frame.Version != e2eeVersion {
		return 0, nil, errors.New("unsupported encrypted frame")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(frame.Ciphertext)
	if err != nil {
		return 0, nil, errors.New("invalid encrypted frame ciphertext")
	}
	return frame.Sequence, ciphertext, nil
}

var (
	e2eeClientProofLabel = []byte("herdr-e2ee-v2 client\x00")
	e2eeServerProofLabel = []byte("herdr-e2ee-v2 server\x00")
	e2eeKeySaltLabel     = []byte("herdr-e2ee-v2 key\x00")
)

type E2EEAuthKind string

const (
	E2EEAuthCredential E2EEAuthKind = "credential"
	E2EEAuthInvitation E2EEAuthKind = "invitation"
)

type E2EEAuthSelector struct {
	Kind    E2EEAuthKind
	ID      string
	Version uint64
	Locale  string
}

type AuthenticatedIdentity struct {
	DeviceID          string `json:"device_id"`
	CredentialID      string `json:"credential_id"`
	Role              string `json:"role"`
	Locale            string `json:"locale"`
	CredentialVersion uint64 `json:"credential_version"`
}

type E2EEAuthResult struct {
	Identity         AuthenticatedIdentity
	CredentialSecret []byte
}

// E2EEAuthResolver is the only authority consulted by the transport. Resolve
// returns a copy of a 32-byte secret. Complete is called with authenticated=false
// for a structurally valid hello whose proof failed, allowing invitation attempt
// limiting. A true completion must revalidate and atomically consume an invite or
// update an existing credential before returning its runtime identity.
type E2EEAuthResolver interface {
	ResolveE2EESecret(context.Context, E2EEAuthSelector) ([]byte, error)
	CompleteE2EEAuth(context.Context, E2EEAuthSelector, bool) (E2EEAuthResult, error)
	IsE2EEAuthRejected(error) bool
}

func isE2EEAuthRejected(resolver E2EEAuthResolver, err error) bool {
	return resolver.IsE2EEAuthRejected(err)
}

type e2eeClientHello struct {
	Type        string       `json:"type"`
	Version     int          `json:"version"`
	AuthKind    E2EEAuthKind `json:"auth_kind"`
	AuthID      string       `json:"auth_id"`
	AuthVersion uint64       `json:"auth_version"`
	Locale      string       `json:"locale"`
	Nonce       string       `json:"nonce"`
	PublicKey   string       `json:"public_key"`
	Proof       string       `json:"proof"`
}

type parsedE2EEClientHello struct {
	selector    E2EEAuthSelector
	nonce       []byte
	publicBytes []byte
	publicKey   *ecdh.PublicKey
	proof       []byte
}

type e2eeServerHello struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"public_key"`
	Proof     string `json:"proof"`
}

type e2eeClientFinish struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
}

type e2eeServerFinish struct {
	Type              string `json:"type"`
	Version           int    `json:"version"`
	DeviceID          string `json:"device_id"`
	CredentialID      string `json:"credential_id"`
	Role              string `json:"role"`
	Locale            string `json:"locale"`
	CredentialVersion uint64 `json:"credential_version"`
	CredentialSecret  string `json:"credential_secret,omitempty"`
}

type e2eeFrame struct {
	Type       string `json:"type"`
	Version    int    `json:"version"`
	Sequence   uint64 `json:"sequence"`
	Ciphertext string `json:"ciphertext"`
}

type e2eeSession struct {
	send             cipher.AEAD
	receive          cipher.AEAD
	codec            FrameCodec
	sendDirection    string
	receiveDirection string
	sendSequence     uint64
	receiveSequence  uint64
}

func performServerE2EEHandshake(parent context.Context, conn FrameConn, resolver E2EEAuthResolver) (*e2eeSession, AuthenticatedIdentity, error) {
	ctx, cancel := context.WithTimeout(parent, e2eeHandshakeTimeout)
	defer cancel()
	if resolver == nil {
		return nil, AuthenticatedIdentity{}, errors.New("device authentication is unavailable")
	}

	rawHello, err := conn.ReadFrame(ctx)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("read client hello: %w", err)
	}
	clientHello, err := parseE2EEClientHello(rawHello)
	if err != nil {
		return nil, AuthenticatedIdentity{}, err
	}
	secret, err := resolver.ResolveE2EESecret(ctx, clientHello.selector)
	if err != nil {
		clear(secret)
		if isE2EEAuthRejected(resolver, err) {
			return nil, AuthenticatedIdentity{}, fmt.Errorf("%w: client proof did not authenticate", ErrDeviceAuthRejected)
		}
		return nil, AuthenticatedIdentity{}, fmt.Errorf("resolve device authentication: %w", err)
	}
	if len(secret) != e2eeSecretBytes {
		clear(secret)
		return nil, AuthenticatedIdentity{}, errors.New("device authentication returned an invalid secret")
	}
	defer clear(secret)

	binding := e2eeAuthBinding(clientHello.selector)
	wantClientProof := e2eeAuthTag(secret, e2eeClientProofLabel, binding, clientHello.nonce, clientHello.publicBytes)
	proofAuthenticated := hmac.Equal(clientHello.proof, wantClientProof)
	clear(wantClientProof)
	if !proofAuthenticated {
		_, completeErr := resolver.CompleteE2EEAuth(ctx, clientHello.selector, false)
		if completeErr != nil && !isE2EEAuthRejected(resolver, completeErr) {
			return nil, AuthenticatedIdentity{}, fmt.Errorf("record rejected device authentication: %w", completeErr)
		}
		return nil, AuthenticatedIdentity{}, fmt.Errorf("%w: client proof did not authenticate", ErrDeviceAuthRejected)
	}

	serverPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("generate server key: %w", err)
	}
	serverNonce := make([]byte, e2eeNonceBytes)
	if _, err := rand.Read(serverNonce); err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("generate server nonce: %w", err)
	}
	serverPublicBytes := serverPrivate.PublicKey().Bytes()
	sharedSecret, err := serverPrivate.ECDH(clientHello.publicKey)
	if err != nil {
		return nil, AuthenticatedIdentity{}, errors.New("derive shared secret")
	}
	defer clear(sharedSecret)

	transcript := e2eeTranscript(binding, clientHello.nonce, clientHello.publicBytes, serverNonce, serverPublicBytes)
	serverProof := e2eeAuthTag(secret, e2eeServerProofLabel, transcript)
	keySalt := e2eeAuthTag(secret, e2eeKeySaltLabel, transcript)
	clientKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 c2s", 32)
	if err != nil {
		clear(keySalt)
		return nil, AuthenticatedIdentity{}, fmt.Errorf("derive client key: %w", err)
	}
	defer clear(clientKey)
	serverKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v2 s2c", 32)
	clear(keySalt)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("derive server key: %w", err)
	}
	defer clear(serverKey)
	session, err := newE2EESession(serverKey, clientKey, e2eeServerDirection, e2eeClientDirection)
	if err != nil {
		return nil, AuthenticatedIdentity{}, err
	}
	session.codec = conn.Codec()

	response, err := json.Marshal(e2eeServerHello{
		Type:      "e2ee_server_hello",
		Version:   e2eeVersion,
		Nonce:     base64.RawURLEncoding.EncodeToString(serverNonce),
		PublicKey: base64.RawURLEncoding.EncodeToString(serverPublicBytes),
		Proof:     base64.RawURLEncoding.EncodeToString(serverProof),
	})
	clear(serverProof)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("encode server hello: %w", err)
	}
	if err := conn.WriteFrame(ctx, response); err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("write server hello: %w", err)
	}
	rawFinish, err := conn.ReadFrame(ctx)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("read client finish: %w", err)
	}
	plaintextFinish, err := session.open(rawFinish)
	if err != nil {
		return nil, AuthenticatedIdentity{}, errors.New("client finish did not authenticate")
	}
	if err := parseE2EEClientFinish(plaintextFinish); err != nil {
		return nil, AuthenticatedIdentity{}, err
	}

	authResult, err := resolver.CompleteE2EEAuth(ctx, clientHello.selector, true)
	if err != nil {
		clear(authResult.CredentialSecret)
		if isE2EEAuthRejected(resolver, err) {
			return nil, AuthenticatedIdentity{}, fmt.Errorf("%w: device authentication is no longer valid", ErrDeviceAuthRejected)
		}
		return nil, AuthenticatedIdentity{}, fmt.Errorf("complete device authentication: %w", err)
	}
	if !validAuthenticatedIdentity(authResult.Identity) {
		clear(authResult.CredentialSecret)
		return nil, AuthenticatedIdentity{}, errors.New("device authentication returned an invalid identity")
	}
	finish := e2eeServerFinish{
		Type:              "e2ee_server_finish",
		Version:           e2eeVersion,
		DeviceID:          authResult.Identity.DeviceID,
		CredentialID:      authResult.Identity.CredentialID,
		Role:              authResult.Identity.Role,
		Locale:            authResult.Identity.Locale,
		CredentialVersion: authResult.Identity.CredentialVersion,
	}
	if len(authResult.CredentialSecret) > 0 {
		if len(authResult.CredentialSecret) != e2eeSecretBytes || clientHello.selector.Kind != E2EEAuthInvitation {
			clear(authResult.CredentialSecret)
			return nil, AuthenticatedIdentity{}, errors.New("invalid issued device credential")
		}
		finish.CredentialSecret = base64.RawURLEncoding.EncodeToString(authResult.CredentialSecret)
		clear(authResult.CredentialSecret)
	}
	encodedFinish, err := json.Marshal(finish)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("encode server finish: %w", err)
	}
	encryptedFinish, err := session.seal(encodedFinish)
	if err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("encrypt server finish: %w", err)
	}
	if err := conn.WriteFrame(ctx, encryptedFinish); err != nil {
		return nil, AuthenticatedIdentity{}, fmt.Errorf("write server finish: %w", err)
	}
	return session, authResult.Identity, nil
}

func parseE2EEClientFinish(plaintext []byte) error {
	if !utf8.Valid(plaintext) {
		return errors.New("invalid client finish")
	}
	var finish e2eeClientFinish
	if err := json.Unmarshal(plaintext, &finish); err != nil || finish.Type != "e2ee_client_finish" || finish.Version != e2eeVersion {
		return errors.New("invalid client finish")
	}
	return nil
}

func parseE2EEClientHello(rawHello []byte) (*parsedE2EEClientHello, error) {
	var hello e2eeClientHello
	if err := json.Unmarshal(rawHello, &hello); err != nil {
		return nil, errors.New("invalid client hello")
	}
	if hello.Type != "e2ee_client_hello" || hello.Version != e2eeVersion {
		return nil, errors.New("unsupported client hello")
	}
	if (hello.AuthKind != E2EEAuthCredential && hello.AuthKind != E2EEAuthInvitation) ||
		hello.AuthID == "" || len(hello.AuthID) > 128 || hello.AuthVersion == 0 || len(hello.Locale) > 32 {
		return nil, errors.New("invalid client authentication selector")
	}
	clientNonce, err := decodeE2EEField(hello.Nonce, e2eeNonceBytes)
	if err != nil {
		return nil, errors.New("invalid client nonce")
	}
	clientPublicBytes, err := decodeE2EEField(hello.PublicKey, e2eePublicKeyBytes)
	if err != nil {
		return nil, errors.New("invalid client public key")
	}
	clientProof, err := decodeE2EEField(hello.Proof, sha256.Size)
	if err != nil {
		return nil, errors.New("invalid client proof")
	}
	clientPublic, err := ecdh.P256().NewPublicKey(clientPublicBytes)
	if err != nil {
		return nil, errors.New("invalid client public key")
	}
	return &parsedE2EEClientHello{
		selector: E2EEAuthSelector{Kind: hello.AuthKind, ID: hello.AuthID, Version: hello.AuthVersion, Locale: hello.Locale},
		nonce:    clientNonce, publicBytes: clientPublicBytes, publicKey: clientPublic, proof: clientProof,
	}, nil
}

func validAuthenticatedIdentity(identity AuthenticatedIdentity) bool {
	if identity.DeviceID == "" || identity.CredentialID == "" || identity.CredentialVersion == 0 || identity.Locale == "" {
		return false
	}
	return identity.Role == "controller" || identity.Role == "reader"
}

func e2eeAuthBinding(selector E2EEAuthSelector) []byte {
	version := strconv.FormatUint(selector.Version, 10)
	binding := make([]byte, 0, len("herdr-e2ee-v2 auth\x00")+len(selector.Kind)+len(selector.ID)+len(version)+3)
	binding = append(binding, "herdr-e2ee-v2 auth\x00"...)
	binding = append(binding, selector.Kind...)
	binding = append(binding, 0)
	binding = append(binding, selector.ID...)
	binding = append(binding, 0)
	binding = append(binding, version...)
	binding = append(binding, 0)
	return binding
}

func newE2EESession(sendKey, receiveKey []byte, sendDirection, receiveDirection string) (*e2eeSession, error) {
	send, err := newE2EEAEAD(sendKey)
	if err != nil {
		return nil, fmt.Errorf("create send cipher: %w", err)
	}
	receive, err := newE2EEAEAD(receiveKey)
	if err != nil {
		return nil, fmt.Errorf("create receive cipher: %w", err)
	}
	return &e2eeSession{send: send, receive: receive, sendDirection: sendDirection, receiveDirection: receiveDirection}, nil
}

func newE2EEAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *e2eeSession) seal(plaintext []byte) ([]byte, error) {
	if s.sendSequence > maxE2EESequence {
		return nil, errors.New("encrypted send sequence exhausted")
	}
	sequence := s.sendSequence
	nonce := e2eeFrameNonce(sequence)
	ciphertext := s.send.Seal(nil, nonce[:], plaintext, e2eeAAD(s.sendDirection, sequence))
	frame, err := s.codec.encodeFrame(sequence, ciphertext)
	if err != nil {
		return nil, err
	}
	s.sendSequence++
	return frame, nil
}

func (s *e2eeSession) open(rawFrame []byte) ([]byte, error) {
	sequence, ciphertext, err := s.codec.decodeFrame(rawFrame)
	if err != nil {
		return nil, err
	}
	if sequence > maxE2EESequence || sequence != s.receiveSequence {
		return nil, errors.New("invalid encrypted frame sequence")
	}
	nonce := e2eeFrameNonce(sequence)
	plaintext, err := s.receive.Open(nil, nonce[:], ciphertext, e2eeAAD(s.receiveDirection, sequence))
	if err != nil {
		return nil, errors.New("encrypted frame authentication failed")
	}
	s.receiveSequence++
	return plaintext, nil
}

func e2eeAuthTag(secret []byte, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, secret)
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	return mac.Sum(nil)
}

func e2eeTranscript(binding, clientNonce, clientPublic, serverNonce, serverPublic []byte) []byte {
	transcript := make([]byte, 0, len(binding)+len(clientNonce)+len(clientPublic)+len(serverNonce)+len(serverPublic))
	transcript = append(transcript, binding...)
	transcript = append(transcript, clientNonce...)
	transcript = append(transcript, clientPublic...)
	transcript = append(transcript, serverNonce...)
	transcript = append(transcript, serverPublic...)
	return transcript
}

func decodeE2EEField(value string, size int) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != size {
		return nil, errors.New("invalid encoded field")
	}
	return decoded, nil
}

func e2eeFrameNonce(sequence uint64) [12]byte {
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], sequence)
	return nonce
}

func e2eeAAD(direction string, sequence uint64) []byte {
	aad := make([]byte, len("herdr-e2ee-v2 ")+len(direction)+1+8)
	position := copy(aad, "herdr-e2ee-v2 ")
	position += copy(aad[position:], direction)
	aad[position] = 0
	binary.BigEndian.PutUint64(aad[position+1:], sequence)
	return aad
}
