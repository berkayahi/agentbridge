package gitbroker

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// ReceiptSigner is deliberately small. Task 14 can bind this interface to an
// enrolled device identity without changing broker operation semantics.
type ReceiptSigner interface {
	Sign([]byte) ([]byte, error)
	Verify(message, signature []byte) error
}

type Ed25519Signer struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func NewEd25519Signer(private ed25519.PrivateKey) (Ed25519Signer, error) {
	if len(private) != ed25519.PrivateKeySize {
		return Ed25519Signer{}, errors.New("gitbroker: invalid Ed25519 private key")
	}
	return Ed25519Signer{private: append(ed25519.PrivateKey(nil), private...), public: private.Public().(ed25519.PublicKey)}, nil
}

func GenerateEd25519Signer() (Ed25519Signer, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Ed25519Signer{}, err
	}
	return Ed25519Signer{private: private, public: public}, nil
}

func (s Ed25519Signer) Sign(message []byte) ([]byte, error) {
	if len(s.private) != ed25519.PrivateKeySize {
		return nil, errors.New("gitbroker: signer is not initialized")
	}
	return ed25519.Sign(s.private, message), nil
}

func (s Ed25519Signer) Verify(message, signature []byte) error {
	if len(s.public) != ed25519.PublicKeySize || !ed25519.Verify(s.public, message, signature) {
		return errors.New("gitbroker: invalid receipt signature")
	}
	return nil
}

// HMACSigner is a local deterministic signer for standalone deployments and
// focused contract checks. Managed mode should replace it with device identity.
type HMACSigner struct{ key []byte }

func NewHMACSigner(key []byte) (HMACSigner, error) {
	if len(key) < 32 {
		return HMACSigner{}, errors.New("gitbroker: HMAC key is too short")
	}
	return HMACSigner{key: append([]byte(nil), key...)}, nil
}

func (s HMACSigner) Sign(message []byte) ([]byte, error) {
	if len(s.key) == 0 {
		return nil, errors.New("gitbroker: signer is not initialized")
	}
	hash := hmac.New(sha256.New, s.key)
	_, _ = hash.Write(message)
	return hash.Sum(nil), nil
}

func (s HMACSigner) Verify(message, signature []byte) error {
	expected, err := s.Sign(message)
	if err != nil || !hmac.Equal(expected, signature) {
		return errors.New("gitbroker: invalid receipt signature")
	}
	return nil
}

var _ ReceiptSigner = Ed25519Signer{}
var _ ReceiptSigner = HMACSigner{}
