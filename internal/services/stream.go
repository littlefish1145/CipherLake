package services

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Streaming SSE-C constants
const (
	ssecChunkSize                   = 32 * 1024
	ssecNonceSize                   = 12
	ssecAuthTagSize                 = 16
	ssecCiphertextLenSize           = 4
	ssecLastChunkMarker      uint32 = 1 << 31
	ssecMetadataOriginalSizeLen     = 8
	envelopeMetadataVersion  byte   = 0x01
)

// deriveSessionKey derives a shared session key from an ECDH exchange.
func deriveSessionKey(clientECDHPriv *ecdh.PrivateKey, serviceECDHPub *ECDHPublicKey) ([]byte, error) {
	servicePub, err := ecdh.P256().NewPublicKey(serviceECDHPub.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse service ECDH public key: %w", err)
	}
	sharedSecret, err := clientECDHPriv.ECDH(servicePub)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}
	defer clearBytes(sharedSecret)

	info := append(servicePub.Bytes(), clientECDHPriv.PublicKey().Bytes()...)
	reader := hkdf.New(sha256.New, sharedSecret, nil, info)
	sessionKey := make([]byte, 32)
	if _, err := io.ReadFull(reader, sessionKey); err != nil {
		return nil, fmt.Errorf("failed to derive session key: %w", err)
	}
	return sessionKey, nil
}

// unwrapDEK decrypts the ECDH-encrypted DEK using the session key.
func unwrapDEK(ecdhEncryptedDEK *ECDHEncryptedDEK, sessionKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}
	dek, err := gcm.Open(nil, ecdhEncryptedDEK.Nonce, ecdhEncryptedDEK.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt DEK: %w", err)
	}
	return dek, nil
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// deriveChunkKey derives a per-chunk encryption key using HKDF-SHA256.
func deriveChunkKey(key []byte, nonce []byte, chunkIdx uint32) []byte {
	info := make([]byte, len(nonce)+4)
	copy(info, nonce)
	binary.BigEndian.PutUint32(info[len(nonce):], chunkIdx)

	h := hmac.New(sha256.New, key)
	h.Reset()
	h.Write(nonce)
	prk := h.Sum(nil)

	expander := hmac.New(sha256.New, prk)
	expander.Write(info)
	expander.Write([]byte{0x01})
	return expander.Sum(nil)
}

// encryptChunk encrypts a plaintext chunk with AES-256-GCM using a derived key.
func encryptChunk(plaintext []byte, key []byte, nonce []byte, chunkIdx uint32, isLast bool) ([]byte, error) {
	derivedKey := deriveChunkKey(key, nonce, chunkIdx)

	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher for chunk %d: %w", chunkIdx, err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM for chunk %d: %w", chunkIdx, err)
	}

	chunkNonce := make([]byte, ssecNonceSize)
	copy(chunkNonce, nonce)
	for i := 0; i < 4; i++ {
		chunkNonce[i] ^= byte(chunkIdx >> (24 - 8*i))
	}

	sealed := gcm.Seal(nil, chunkNonce, plaintext, nil)

	if len(sealed) < ssecAuthTagSize {
		return nil, fmt.Errorf("sealed chunk too short")
	}
	ciphertext := sealed[:len(sealed)-ssecAuthTagSize]
	authTag := sealed[len(sealed)-ssecAuthTagSize:]

	ctLen := uint32(len(ciphertext))
	if isLast {
		ctLen |= ssecLastChunkMarker
	}

	out := make([]byte, 0, ssecCiphertextLenSize+len(ciphertext)+ssecAuthTagSize)
	out = binary.BigEndian.AppendUint32(out, ctLen)
	out = append(out, ciphertext...)
	out = append(out, authTag...)

	return out, nil
}

// streamingEncryptReader implements io.Reader for streaming SSE-C encryption.
type streamingEncryptReader struct {
	source   io.Reader
	key      []byte
	nonce    []byte
	chunkIdx uint32
	buf      []byte
	outBuf   *bytes.Buffer
	done     bool
}

func newStreamingEncryptReader(source io.Reader, key []byte, nonce []byte) *streamingEncryptReader {
	return &streamingEncryptReader{
		source: source,
		key:    key,
		nonce:  nonce,
		buf:    make([]byte, ssecChunkSize),
		outBuf: new(bytes.Buffer),
	}
}

func (r *streamingEncryptReader) Read(p []byte) (int, error) {
	for {
		if r.outBuf.Len() > 0 {
			return r.outBuf.Read(p)
		}

		if r.done {
			return 0, io.EOF
		}

		n, readErr := io.ReadFull(r.source, r.buf)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return 0, readErr
		}

		isLast := readErr == io.EOF || readErr == io.ErrUnexpectedEOF || n == 0
		if n == 0 && isLast {
			if r.chunkIdx == 0 {
				chunkData, err := encryptChunk(nil, r.key, r.nonce, r.chunkIdx, true)
				if err != nil {
					return 0, err
				}
				r.outBuf.Write(chunkData)
				r.done = true
				continue
			}
			r.done = true
			continue
		}

		plaintext := r.buf[:n]

		chunkData, err := encryptChunk(plaintext, r.key, r.nonce, r.chunkIdx, isLast)
		if err != nil {
			return 0, err
		}

		r.chunkIdx++
		r.outBuf.Write(chunkData)

		if isLast {
			r.done = true
		}
	}
}

// streamingDecryptReader implements io.Reader for streaming decryption.
type streamingDecryptReader struct {
	source   io.Reader
	key      []byte
	nonce    []byte
	chunkIdx uint32
	outBuf   *bytes.Buffer
	done     bool
	lenBuf   [ssecCiphertextLenSize]byte
	lenRead  int
}

func newStreamingDecryptReader(source io.Reader, key []byte, nonce []byte) *streamingDecryptReader {
	return &streamingDecryptReader{
		source: source,
		key:    key,
		nonce:  nonce,
		outBuf: new(bytes.Buffer),
	}
}

func (r *streamingDecryptReader) Read(p []byte) (int, error) {
	for {
		if r.outBuf.Len() > 0 {
			return r.outBuf.Read(p)
		}

		if r.done {
			return 0, io.EOF
		}

		for r.lenRead < ssecCiphertextLenSize {
			n, err := r.source.Read(r.lenBuf[r.lenRead:])
			if n > 0 {
				r.lenRead += n
			}
			if err != nil {
				if err == io.EOF && r.lenRead > 0 {
					return 0, fmt.Errorf("truncated chunk header")
				}
				return 0, err
			}
		}
		r.lenRead = 0

		ctLenField := binary.BigEndian.Uint32(r.lenBuf[:])
		isLast := (ctLenField & ssecLastChunkMarker) != 0
		ctLen := int(ctLenField & ^ssecLastChunkMarker)

		chunkData := make([]byte, ctLen+ssecAuthTagSize)
		if _, err := io.ReadFull(r.source, chunkData); err != nil {
			return 0, fmt.Errorf("failed to read chunk data: %w", err)
		}

		ciphertext := chunkData[:ctLen]
		authTag := chunkData[ctLen:]

		derivedKey := deriveChunkKey(r.key, r.nonce, r.chunkIdx)

		block, err := aes.NewCipher(derivedKey)
		if err != nil {
			return 0, fmt.Errorf("decryption failed")
		}

		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return 0, fmt.Errorf("decryption failed")
		}

		gcmData := append(ciphertext, authTag...)

		chunkNonce := make([]byte, ssecNonceSize)
		copy(chunkNonce, r.nonce)
		for i := 0; i < 4; i++ {
			chunkNonce[i] ^= byte(r.chunkIdx >> (24 - 8*i))
		}

		plaintext, err := gcm.Open(nil, chunkNonce, gcmData, nil)
		if err != nil {
			return 0, fmt.Errorf("decryption failed")
		}

		r.chunkIdx++
		r.outBuf.Write(plaintext)

		if isLast {
			r.done = true
		}
	}
}

// ComputeSSECKeySHA256 computes the SHA-256 hash of a client key for storage verification.
func ComputeSSECKeySHA256(clientKey []byte) string {
	h := sha256.Sum256(clientKey)
	return fmt.Sprintf("%x", h[:])
}
