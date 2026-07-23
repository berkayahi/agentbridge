// Package deviceidentity owns the device signing key and its public identity.
package deviceidentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var ErrInvalidKey = errors.New("device identity: invalid key")

type Key struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func Generate() (Key, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, err
	}
	return Key{private: private, public: public}, nil
}

func FromPrivate(private []byte) (Key, error) {
	if len(private) != ed25519.PrivateKeySize {
		return Key{}, ErrInvalidKey
	}
	value := append([]byte(nil), private...)
	key := ed25519.PrivateKey(value)
	public := append([]byte(nil), key.Public().(ed25519.PublicKey)...)
	return Key{private: key, public: ed25519.PublicKey(public)}, nil
}

func FromPublic(public []byte) (Key, error) {
	if len(public) != ed25519.PublicKeySize {
		return Key{}, ErrInvalidKey
	}
	return Key{public: ed25519.PublicKey(append([]byte(nil), public...))}, nil
}

func (k Key) PublicKey() []byte { return append([]byte(nil), k.public...) }

func (k Key) PrivateKey() []byte { return append([]byte(nil), k.private...) }

func (k Key) HasPrivate() bool { return len(k.private) == ed25519.PrivateKeySize }

func (k Key) Fingerprint() string {
	digest := sha256.Sum256(k.public)
	return hex.EncodeToString(digest[:])
}

func (k Key) Sign(message []byte) ([]byte, error) {
	if !k.HasPrivate() {
		return nil, ErrInvalidKey
	}
	return ed25519.Sign(k.private, message), nil
}

func (k Key) Verify(message, signature []byte) bool {
	return len(k.public) == ed25519.PublicKeySize && ed25519.Verify(k.public, message, signature)
}
