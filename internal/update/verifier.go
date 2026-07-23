package update

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"
)

var ErrTrust = errors.New("update: trusted release signature threshold not met")

type TrustRoot struct {
	Keys      map[string]ed25519.PublicKey
	Threshold int
}

func (r TrustRoot) Validate() error {
	if len(r.Keys) == 0 || r.Threshold <= 0 || r.Threshold > len(r.Keys) {
		return ErrTrust
	}
	for id, key := range r.Keys {
		if id == "" || len(key) != ed25519.PublicKeySize {
			return ErrTrust
		}
	}
	return nil
}

func Verify(now time.Time, metadata Metadata, root TrustRoot, floor Floor) error {
	if err := metadata.Validate(now); err != nil {
		return err
	}
	if err := root.Validate(); err != nil {
		return err
	}
	if metadata.Version <= floor.MetadataVersion || metadata.Version < floor.MetadataVersion {
		return ErrRollback
	}
	canonical, err := metadata.CanonicalBytes()
	if err != nil {
		return err
	}
	valid := 0
	seen := make(map[string]struct{})
	for _, id := range metadata.SignerIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		key, ok := root.Keys[id]
		if ok && ed25519.Verify(key, canonical, metadata.Signatures[id]) {
			valid++
		}
	}
	if valid < root.Threshold {
		return fmt.Errorf("%w: got %d of %d", ErrTrust, valid, root.Threshold)
	}
	return nil
}
