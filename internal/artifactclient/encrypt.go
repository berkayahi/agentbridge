package artifactclient

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"time"
)

var ErrInvalidKey = errors.New("artifact: invalid encryption key")

func Encrypt(grant Grant, key []byte, plaintext []byte, now time.Time) (EncryptedArtifact, error) {
	if err := grant.Validate(now); err != nil {
		return EncryptedArtifact{}, err
	}
	if len(key) != 32 {
		return EncryptedArtifact{}, ErrInvalidKey
	}
	if int64(len(plaintext)) != grant.SizeBytes || digestBytes(plaintext) != grant.PlaintextDigest {
		return EncryptedArtifact{}, ErrConflict
	}
	keyCopy := append([]byte(nil), key...)
	defer clearBytes(keyCopy)
	block, err := aes.NewCipher(keyCopy)
	if err != nil {
		return EncryptedArtifact{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return EncryptedArtifact{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return EncryptedArtifact{}, err
	}
	associatedData, err := grant.CanonicalBytes()
	if err != nil {
		return EncryptedArtifact{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, associatedData)
	grantDigest, err := grant.Digest()
	if err != nil {
		return EncryptedArtifact{}, err
	}
	envelope := append(append([]byte(nil), nonce...), ciphertext...)
	return EncryptedArtifact{ArtifactID: grant.ArtifactID, ObjectKey: grant.ObjectKey, GrantNonce: grant.Nonce, GrantDigest: grantDigest, Algorithm: grant.Algorithm, KeyID: grant.KeyID, Nonce: nonce, Ciphertext: ciphertext, PlaintextDigest: grant.PlaintextDigest, EnvelopeDigest: digestBytes(envelope), SizeBytes: int64(len(plaintext))}, nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
