package deviceidentity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var ErrInsecureKeyFile = errors.New("device identity: key file is not owner-only")

type storedKey struct {
	Version     int    `json:"version"`
	Private     string `json:"private_key"`
	Public      string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

func Save(path string, key Key) error {
	if path == "" || !key.HasPrivate() || len(key.public) != ed25519.PublicKeySize {
		return ErrInvalidKey
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect identity directory: %w", err)
	}
	value, err := json.Marshal(storedKey{
		Version: 1, Private: base64.RawStdEncoding.EncodeToString(key.private),
		Public: base64.RawStdEncoding.EncodeToString(key.public), Fingerprint: key.Fingerprint(),
	})
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".device-key-*")
	if err != nil {
		return err
	}
	tmpPath := temporary.Name()
	defer os.Remove(tmpPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install device key: %w", err)
	}
	return nil
}

func Load(path string) (Key, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Key{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Key{}, ErrInsecureKeyFile
	}
	file, err := os.Open(path)
	if err != nil {
		return Key{}, err
	}
	defer file.Close()
	var stored storedKey
	decoder := json.NewDecoder(io.LimitReader(file, 16*1024))
	if err := decoder.Decode(&stored); err != nil {
		return Key{}, fmt.Errorf("decode device key: %w", err)
	}
	if stored.Version != 1 {
		return Key{}, ErrInvalidKey
	}
	private, err := base64.RawStdEncoding.DecodeString(stored.Private)
	if err != nil {
		return Key{}, ErrInvalidKey
	}
	key, err := FromPrivate(private)
	if err != nil {
		return Key{}, err
	}
	if stored.Public != base64.RawStdEncoding.EncodeToString(key.public) || stored.Fingerprint != key.Fingerprint() {
		return Key{}, ErrInvalidKey
	}
	return key, nil
}
