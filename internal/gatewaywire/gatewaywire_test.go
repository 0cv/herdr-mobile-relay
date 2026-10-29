package gatewaywire

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"testing"
)

func TestRegistrationProof(t *testing.T) {
	nonce := bytes.Repeat([]byte{7}, NonceBytes)
	proof := RegistrationProof("secret", "abcdefghijklmnopqrstuv", nonce)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("herdr-gateway-register-v1abcdefghijklmnopqrstuv"))
	mac.Write(nonce)
	if !hmac.Equal(proof, mac.Sum(nil)) {
		t.Fatal("proof does not match registration wire contract")
	}
	for _, tc := range []struct {
		name, token, relayID string
		nonce, proof         []byte
		want                 bool
	}{
		{"valid", "secret", "abcdefghijklmnopqrstuv", nonce, proof, true},
		{"wrong token", "wrong", "abcdefghijklmnopqrstuv", nonce, proof, false},
		{"wrong relay", "secret", "other", nonce, proof, false},
		{"wrong nonce", "secret", "abcdefghijklmnopqrstuv", bytes.Repeat([]byte{8}, NonceBytes), proof, false},
		{"short nonce", "secret", "abcdefghijklmnopqrstuv", nonce[:1], proof, false},
		{"missing proof", "secret", "abcdefghijklmnopqrstuv", nonce, nil, false},
		{"short proof", "secret", "abcdefghijklmnopqrstuv", nonce, proof[:1], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifyRegistrationProof(tc.token, tc.relayID, tc.nonce, tc.proof); got != tc.want {
				t.Fatalf("verify = %v, want %v", got, tc.want)
			}
		})
	}
}
