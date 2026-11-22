// Package sealbox encrypts small secrets at rest (TOTP secrets, SMTP and
// Twilio credentials) with AES-256-GCM, so a database dump alone doesn't
// reveal them. Associated data binds a sealed value to where it belongs: a
// value copied to another row or setting fails to open.
package sealbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Derive returns a 32-byte subkey of master for one purpose, so one backed-up
// key (PARLEY_MFA_KEY) can protect several kinds of data independently.
func Derive(master []byte, purpose string) ([]byte, error) {
	if len(master) != 32 {
		return nil, errors.New("sealbox: master key must be 32 bytes")
	}
	return hkdf.Key(sha256.New, master, nil, "parley sealbox v1: "+purpose, 32)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext under key, authenticated with aad.
func Seal(key, aad, plaintext []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, plaintext, aad), nil
}

// Open reverses Seal.
func Open(key, aad, sealed []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < g.NonceSize() {
		return nil, errors.New("sealbox: malformed value")
	}
	return g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], aad)
}
