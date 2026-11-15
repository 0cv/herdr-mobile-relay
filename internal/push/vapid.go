package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

func (m *Manager) loadOrGenerateVAPIDKeys(pushDir string) error {
	privPath := filepath.Join(pushDir, "vapid_private.pem")
	pubPath := filepath.Join(pushDir, "vapid_public.pem")

	privData, privErr := os.ReadFile(privPath)
	pubData, pubErr := os.ReadFile(pubPath)

	if privErr != nil && !os.IsNotExist(privErr) {
		return fmt.Errorf("read VAPID private key: %w", privErr)
	}
	if pubErr != nil && !os.IsNotExist(pubErr) {
		return fmt.Errorf("read VAPID public key: %w", pubErr)
	}

	privMissing := os.IsNotExist(privErr)
	pubMissing := os.IsNotExist(pubErr)
	if privMissing && !pubMissing {
		return fmt.Errorf("VAPID private key is missing while public key exists")
	}
	var privateKey *ecdsa.PrivateKey
	var publicKey *ecdsa.PublicKey
	if !privMissing {
		var err error
		privateKey, err = parseVAPIDPrivateKey(string(privData))
		if err != nil {
			m.logger.Warn("parse VAPID private key; generating new key pair", "error", err)
		}
	}
	if !pubMissing {
		var err error
		publicKey, err = parseVAPIDPublicKey(string(pubData))
		if err != nil {
			m.logger.Warn("parse VAPID public key; generating new key pair", "error", err)
		}
	}
	if privateKey != nil && (pubMissing || publicKey != nil) {
		derivedPublic := deriveVAPIDPublic(privateKey)

		if pubMissing {
			publicDER, err := x509.MarshalPKIXPublicKey(derivedPublic)
			if err != nil {
				return fmt.Errorf("encode derived VAPID public key: %w", err)
			}
			publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
			if err := atomicWrite(pubPath, publicPEM, 0o644); err != nil {
				return fmt.Errorf("write derived VAPID public key: %w", err)
			}
			m.logger.Info("derived missing VAPID public key from existing private key")
		} else {
			if publicKey.X.Cmp(derivedPublic.X) != 0 || publicKey.Y.Cmp(derivedPublic.Y) != 0 {
				return fmt.Errorf("VAPID public key does not match private key")
			}
		}

		info, err := os.Stat(privPath)
		if err != nil {
			return fmt.Errorf("stat VAPID private key: %w", err)
		}
		if info.Mode().Perm()&^0o600 != 0 {
			if err := os.Chmod(privPath, info.Mode().Perm()&0o600); err != nil {
				return fmt.Errorf("protect VAPID private key: %w", err)
			}
		}
		m.vapidPrivate = encodeVAPIDPrivate(privateKey)
		m.vapidPublic = encodeVAPIDPublic(derivedPublic)
		return nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate VAPID keys: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode VAPID private key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("encode VAPID public key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := atomicWrite(privPath, privatePEM, 0o600); err != nil {
		return fmt.Errorf("write VAPID private key: %w", err)
	}
	if err := atomicWrite(pubPath, publicPEM, 0o644); err != nil {
		return fmt.Errorf("write VAPID public key: %w", err)
	}

	m.vapidPrivate = encodeVAPIDPrivate(key)
	m.vapidPublic = encodeVAPIDPublic(&key.PublicKey)
	m.logger.Info("generated new VAPID key pair")
	return nil
}

func parseVAPIDPrivateKey(data string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM block")
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an EC private key")
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("VAPID private key must use P-256")
	}
	return key, nil
}

func parseVAPIDPublicKey(data string) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM block")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}

	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not an EC public key")
	}
	if ecPub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("VAPID public key must use P-256")
	}
	return ecPub, nil
}

func deriveVAPIDPublic(key *ecdsa.PrivateKey) *ecdsa.PublicKey {
	x, y := elliptic.P256().ScalarBaseMult(key.D.Bytes())
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
}

func encodeVAPIDPrivate(key *ecdsa.PrivateKey) string {
	privateScalar := make([]byte, 32)
	key.D.FillBytes(privateScalar)
	return base64.RawURLEncoding.EncodeToString(privateScalar)
}

func encodeVAPIDPublic(key *ecdsa.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(elliptic.Marshal(elliptic.P256(), key.X, key.Y))
}
