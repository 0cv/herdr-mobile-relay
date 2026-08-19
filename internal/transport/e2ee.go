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
	"time"
	"unicode/utf8"

	relayprotocol "github.com/0cv/herdr-mobile-relay/internal/protocol"
)

const (
	e2eeSubprotocol             = relayprotocol.EncryptedWebSocketSubprotocol
	e2eeVersion                 = 1
	e2eeHandshakeTimeout        = 10 * time.Second
	e2eeNonceBytes              = 32
	e2eePublicKeyBytes          = 65
	maxE2EESequence      uint64 = 1<<53 - 1

	e2eeClientDirection = "c2s"
	e2eeServerDirection = "s2c"
)

// FrameCodec selects how an encrypted frame is encoded on a transport.
type FrameCodec int

const (
	// CodecJSON wraps the ciphertext in a JSON envelope with a base64url
	// field. It is the original browser WebSocket encoding.
	CodecJSON FrameCodec = iota
	// CodecBinary prefixes raw ciphertext with a fixed header. The gateway
	// and WebRTC paths use it to drop the base64 (+33 %) expansion and the
	// JSON envelope of CodecJSON.
	CodecBinary
)

const (
	binaryFrameVersion    = 1
	binaryFrameKindData   = 0
	binaryFrameHeaderSize = 1 + 1 + 8
)

// encodeFrame renders one encrypted frame for the wire.
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

// decodeFrame parses one encrypted frame from the wire.
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
	e2eeClientProofLabel = []byte("herdr-e2ee-v1 client\x00")
	e2eeServerProofLabel = []byte("herdr-e2ee-v1 server\x00")
	e2eeKeySaltLabel     = []byte("herdr-e2ee-v1 key\x00")
)

type e2eeClientHello struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"public_key"`
	Proof     string `json:"proof"`
}

type parsedE2EEClientHello struct {
	nonce       []byte
	publicBytes []byte
	publicKey   *ecdh.PublicKey
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

func performServerE2EEHandshake(parent context.Context, conn FrameConn, token string) (*e2eeSession, error) {
	ctx, cancel := context.WithTimeout(parent, e2eeHandshakeTimeout)
	defer cancel()

	rawHello, err := conn.ReadFrame(ctx)
	if err != nil {
		return nil, fmt.Errorf("read client hello: %w", err)
	}
	clientHello, err := parseE2EEClientHello(rawHello, token)
	if err != nil {
		return nil, err
	}
	serverPrivate, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}
	serverNonce := make([]byte, e2eeNonceBytes)
	if _, err := rand.Read(serverNonce); err != nil {
		return nil, fmt.Errorf("generate server nonce: %w", err)
	}
	serverPublicBytes := serverPrivate.PublicKey().Bytes()
	sharedSecret, err := serverPrivate.ECDH(clientHello.publicKey)
	if err != nil {
		return nil, errors.New("derive shared secret")
	}
	defer clear(sharedSecret)

	transcript := e2eeTranscript(clientHello.nonce, clientHello.publicBytes, serverNonce, serverPublicBytes)
	serverProof := e2eeAuthTag(token, e2eeServerProofLabel, transcript)
	keySalt := e2eeAuthTag(token, e2eeKeySaltLabel, transcript)
	clientKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v1 c2s", 32)
	if err != nil {
		return nil, fmt.Errorf("derive client key: %w", err)
	}
	defer clear(clientKey)
	serverKey, err := hkdf.Key(sha256.New, sharedSecret, keySalt, "herdr-e2ee-v1 s2c", 32)
	if err != nil {
		return nil, fmt.Errorf("derive server key: %w", err)
	}
	defer clear(serverKey)
	session, err := newE2EESession(serverKey, clientKey, e2eeServerDirection, e2eeClientDirection)
	if err != nil {
		return nil, err
	}
	session.codec = conn.Codec()

	response, err := json.Marshal(e2eeServerHello{
		Type:      "e2ee_server_hello",
		Version:   e2eeVersion,
		Nonce:     base64.RawURLEncoding.EncodeToString(serverNonce),
		PublicKey: base64.RawURLEncoding.EncodeToString(serverPublicBytes),
		Proof:     base64.RawURLEncoding.EncodeToString(serverProof),
	})
	if err != nil {
		return nil, fmt.Errorf("encode server hello: %w", err)
	}
	if err := conn.WriteFrame(ctx, response); err != nil {
		return nil, fmt.Errorf("write server hello: %w", err)
	}
	rawFinish, err := conn.ReadFrame(ctx)
	if err != nil {
		return nil, fmt.Errorf("read client finish: %w", err)
	}
	plaintextFinish, err := session.open(rawFinish)
	if err != nil {
		return nil, errors.New("client finish did not authenticate")
	}
	if err := parseE2EEClientFinish(plaintextFinish); err != nil {
		return nil, err
	}
	return session, nil
}

func parseE2EEClientFinish(plaintext []byte) error {
	if !utf8.Valid(plaintext) {
		return errors.New("invalid client finish")
	}
	var finish e2eeClientFinish
	if err := json.Unmarshal(plaintext, &finish); err != nil ||
		finish.Type != "e2ee_client_finish" ||
		finish.Version != e2eeVersion {
		return errors.New("invalid client finish")
	}
	return nil
}

func parseE2EEClientHello(rawHello []byte, token string) (*parsedE2EEClientHello, error) {
	var hello e2eeClientHello
	if err := json.Unmarshal(rawHello, &hello); err != nil {
		return nil, errors.New("invalid client hello")
	}
	if hello.Type != "e2ee_client_hello" || hello.Version != e2eeVersion {
		return nil, errors.New("unsupported client hello")
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
	wantClientProof := e2eeAuthTag(token, e2eeClientProofLabel, clientNonce, clientPublicBytes)
	if !hmac.Equal(clientProof, wantClientProof) {
		return nil, errors.New("client proof did not authenticate")
	}
	clientPublic, err := ecdh.P256().NewPublicKey(clientPublicBytes)
	if err != nil {
		return nil, errors.New("invalid client public key")
	}
	return &parsedE2EEClientHello{
		nonce:       clientNonce,
		publicBytes: clientPublicBytes,
		publicKey:   clientPublic,
	}, nil
}

// newE2EESession builds a directional AEAD pair. The zero FrameCodec is
// CodecJSON, the browser WebSocket encoding; transports that carry raw
// ciphertext set codec after construction.
func newE2EESession(sendKey, receiveKey []byte, sendDirection, receiveDirection string) (*e2eeSession, error) {
	send, err := newE2EEAEAD(sendKey)
	if err != nil {
		return nil, fmt.Errorf("create send cipher: %w", err)
	}
	receive, err := newE2EEAEAD(receiveKey)
	if err != nil {
		return nil, fmt.Errorf("create receive cipher: %w", err)
	}
	return &e2eeSession{
		send:             send,
		receive:          receive,
		sendDirection:    sendDirection,
		receiveDirection: receiveDirection,
	}, nil
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

func e2eeAuthTag(token string, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	return mac.Sum(nil)
}

func e2eeTranscript(clientNonce, clientPublic, serverNonce, serverPublic []byte) []byte {
	transcript := make([]byte, 0, len(clientNonce)+len(clientPublic)+len(serverNonce)+len(serverPublic))
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
	aad := make([]byte, len("herdr-e2ee-v1 ")+len(direction)+1+8)
	position := copy(aad, "herdr-e2ee-v1 ")
	position += copy(aad[position:], direction)
	aad[position] = 0
	binary.BigEndian.PutUint64(aad[position+1:], sequence)
	return aad
}
