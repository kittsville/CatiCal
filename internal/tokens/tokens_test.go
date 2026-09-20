package tokens

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestHash_SameSaltAndSecret(t *testing.T) {
	salt := bytes.Repeat([]byte{0x11}, SaltSize)
	secret := bytes.Repeat([]byte{0x22}, SecretSize)

	got := Hash(salt, secret)
	want := sha256.Sum256(append(append([]byte{}, salt...), secret...))
	if !bytes.Equal(got, want[:]) {
		t.Fatalf("Hash() = %x, want %x", got, want[:])
	}

	again := Hash(salt, secret)
	if !bytes.Equal(got, again) {
		t.Fatalf("Hash() not deterministic: %x vs %x", got, again)
	}
}

func TestVerify(t *testing.T) {
	salt := bytes.Repeat([]byte{0xab}, SaltSize)
	secret := bytes.Repeat([]byte{0xcd}, SecretSize)
	hash := Hash(salt, secret)

	wrongSecret := append([]byte{}, secret...)
	wrongSecret[0] ^= 0xff

	mutatedSalt := append([]byte{}, salt...)
	mutatedSalt[0] ^= 0xff

	tests := []struct {
		name   string
		salt   []byte
		hash   []byte
		secret []byte
		want   bool
	}{
		{name: "match", salt: salt, hash: hash, secret: secret, want: true},
		{name: "wrong secret", salt: salt, hash: hash, secret: wrongSecret, want: false},
		{name: "mutated salt", salt: mutatedSalt, hash: hash, secret: secret, want: false},
		{name: "nil salt", salt: nil, hash: hash, secret: secret, want: false},
		{name: "nil hash", salt: salt, hash: nil, secret: secret, want: false},
		{name: "nil secret", salt: salt, hash: hash, secret: nil, want: false},
		{name: "short salt", salt: salt[:8], hash: hash, secret: secret, want: false},
		{name: "short hash", salt: salt, hash: hash[:8], secret: secret, want: false},
		{name: "short secret", salt: salt, hash: hash, secret: secret[:8], want: false},
		{name: "empty slices", salt: []byte{}, hash: []byte{}, secret: []byte{}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Verify(tt.salt, tt.hash, tt.secret)
			if got != tt.want {
				t.Fatalf("Verify() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGenerate_SizesAndNotAllZero(t *testing.T) {
	salt, secret, hash, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(salt) != SaltSize {
		t.Fatalf("salt len = %d, want %d", len(salt), SaltSize)
	}
	if len(secret) != SecretSize {
		t.Fatalf("secret len = %d, want %d", len(secret), SecretSize)
	}
	if len(hash) != sha256.Size {
		t.Fatalf("hash len = %d, want %d", len(hash), sha256.Size)
	}
	if bytes.Equal(salt, make([]byte, SaltSize)) {
		t.Fatal("salt is all-zero")
	}
	if bytes.Equal(secret, make([]byte, SecretSize)) {
		t.Fatal("secret is all-zero")
	}
	if !Verify(salt, hash, secret) {
		t.Fatal("Generate() material failed Verify")
	}
}
