// Package tokens generates and verifies capability secrets.
//
// Hash is SHA-256(salt || secret). Salt is 16 bytes and the secret is 32 bytes
// from crypto/rand. The secret is never stored; only salt and hash are.
//
// URL form for both feed and manage links is /{kind}/{id}/{secret} where id is
// the feed UUID (plaintext primary key). Lookup is by id, then Verify.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
)

const (
	SaltSize   = 16
	SecretSize = 32
)

// Generate returns a fresh salt, secret, and SHA-256(salt || secret).
func Generate() (salt, secret, hash []byte, err error) {
	salt = make([]byte, SaltSize)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, nil, err
	}
	secret = make([]byte, SecretSize)
	if _, err = rand.Read(secret); err != nil {
		return nil, nil, nil, err
	}
	return salt, secret, Hash(salt, secret), nil
}

// Hash returns SHA-256(salt || secret).
func Hash(salt, secret []byte) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write(secret)
	return h.Sum(nil)
}

// Verify reports whether hash equals SHA-256(salt || secret) using a
// constant-time compare. Short or nil slices return false and do not panic.
func Verify(salt, hash, secret []byte) bool {
	if len(salt) != SaltSize || len(secret) != SecretSize || len(hash) != sha256.Size {
		return false
	}
	want := Hash(salt, secret)
	return subtle.ConstantTimeCompare(hash, want) == 1
}
