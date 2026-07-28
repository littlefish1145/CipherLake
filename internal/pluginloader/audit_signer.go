package pluginloader

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// ECDSAAuditSigner signs audit payloads using an ECDSA private key.
// It is used by the crypto.audit.sign Tier-2-only host import (spec §3.7 A4, P5-5).
type ECDSAAuditSigner struct {
	privateKey *ecdsa.PrivateKey
	publicKey  []byte
	keyID      string
}

// NewECDSAAuditSigner loads or generates an ECDSA P-256 key pair at keyPath.
// The private key is stored at keyPath.pem and the public key at keyPath.pub.
func NewECDSAAuditSigner(keyPath, keyID string) (*ECDSAAuditSigner, error) {
	if keyPath == "" {
		keyPath = "./data/keys/audit"
	}
	privPath := keyPath + ".pem"
	pubPath := keyPath + ".pub"

	var priv *ecdsa.PrivateKey
	if data, err := os.ReadFile(privPath); err == nil {
		block, _ := pem.Decode(data)
		if block != nil {
			parsed, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse audit private key: %w", err)
			}
			priv = parsed
		}
	}

	if priv == nil {
		var err error
		priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate audit key pair: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(privPath), 0700); err != nil {
			return nil, fmt.Errorf("create audit key directory: %w", err)
		}
		privBytes, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			return nil, fmt.Errorf("marshal audit private key: %w", err)
		}
		block := &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}
		if err := os.WriteFile(privPath, pem.EncodeToMemory(block), 0600); err != nil {
			return nil, fmt.Errorf("write audit private key: %w", err)
		}

		pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("marshal audit public key: %w", err)
		}
		if err := os.WriteFile(pubPath, pubBytes, 0644); err != nil {
			return nil, fmt.Errorf("write audit public key: %w", err)
		}
	}

	pubBytes, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	return &ECDSAAuditSigner{
		privateKey: priv,
		publicKey:  pubBytes,
		keyID:      keyID,
	}, nil
}

// Sign signs the payload with ECDSA-SHA256 and returns the signature as
// base64(R || S), where R and S are fixed-width big-endian integers.
func (s *ECDSAAuditSigner) Sign(payload []byte) ([]byte, error) {
	if s.privateKey == nil {
		return nil, fmt.Errorf("audit signer has no private key")
	}
	digest := sha256.Sum256(payload)
	r, sigS, err := ecdsa.Sign(rand.Reader, s.privateKey, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign audit payload: %w", err)
	}
	curveByteLen := (s.privateKey.Curve.Params().BitSize + 7) / 8
	sig := make([]byte, 2*curveByteLen)
	r.FillBytes(sig[:curveByteLen])
	sigS.FillBytes(sig[curveByteLen:])
	return []byte(base64.StdEncoding.EncodeToString(sig)), nil
}

// PublicKey returns the PKIX-encoded public key and key ID.
func (s *ECDSAAuditSigner) PublicKey() ([]byte, string) {
	return s.publicKey, s.keyID
}
