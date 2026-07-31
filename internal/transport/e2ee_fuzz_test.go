package transport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func FuzzE2EEClientHello(f *testing.F) {
	rawVector, err := os.ReadFile("../../contracts/fixtures/e2ee/v1.json")
	if err != nil {
		f.Fatal(err)
	}
	var vector e2eeVector
	if err := json.Unmarshal(rawVector, &vector); err != nil {
		f.Fatal(err)
	}
	validHello, err := json.Marshal(vector.Client.Hello)
	if err != nil {
		f.Fatal(err)
	}

	invalidPoint := make([]byte, e2eePublicKeyBytes)
	invalidPoint[0] = 0x04
	invalidNonce := make([]byte, e2eeNonceBytes)
	invalidPointHello, err := json.Marshal(e2eeClientHello{
		Type:      "e2ee_client_hello",
		Version:   e2eeVersion,
		Nonce:     base64.RawURLEncoding.EncodeToString(invalidNonce),
		PublicKey: base64.RawURLEncoding.EncodeToString(invalidPoint),
		Proof: base64.RawURLEncoding.EncodeToString(
			e2eeAuthTag(vector.RelayKey, e2eeClientProofLabel, invalidNonce, invalidPoint),
		),
	})
	if err != nil {
		f.Fatal(err)
	}

	f.Add(validHello)
	f.Add(invalidPointHello)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":"e2ee_client_hello","version":1}`))
	f.Add([]byte(`{"type":"e2ee_client_hello","version":1,"nonce":"%%%","public_key":"","proof":""}`))
	f.Add([]byte(`{"type":"e2ee_client_hello","version":1,"nonce":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","proof":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`))

	f.Fuzz(func(t *testing.T, rawHello []byte) {
		hello, err := parseE2EEClientHello(rawHello, vector.RelayKey)
		if err != nil {
			return
		}
		if len(hello.nonce) != e2eeNonceBytes {
			t.Fatalf("accepted nonce length %d", len(hello.nonce))
		}
		if len(hello.publicBytes) != e2eePublicKeyBytes || hello.publicKey == nil {
			t.Fatal("accepted invalid public key")
		}
	})
}

func FuzzE2EEEncryptedEnvelope(f *testing.F) {
	clientKey := bytes.Repeat([]byte{0x11}, 32)
	serverKey := bytes.Repeat([]byte{0x22}, 32)
	sender, err := newE2EESession(clientKey, serverKey, e2eeClientDirection, e2eeServerDirection)
	if err != nil {
		f.Fatal(err)
	}
	validFrame, err := sender.seal([]byte(`{"type":"refresh_agents"}`))
	if err != nil {
		f.Fatal(err)
	}

	f.Add(validFrame)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":"e2ee","version":1,"sequence":-1,"ciphertext":""}`))
	f.Add([]byte(`{"type":"e2ee","version":1,"sequence":9007199254740992,"ciphertext":""}`))
	f.Add([]byte(`{"type":"e2ee","version":1,"sequence":0,"ciphertext":"%%%"}`))

	f.Fuzz(func(t *testing.T, rawFrame []byte) {
		receiver, err := newE2EESession(serverKey, clientKey, e2eeServerDirection, e2eeClientDirection)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = receiver.open(rawFrame)
	})
}
