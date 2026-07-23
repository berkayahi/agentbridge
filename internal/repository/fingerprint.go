package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

var ErrUnstableIdentity = errors.New("repository: stable filesystem identity is unavailable")

// fileIdentity is deliberately derived from the operating system's file
// identity rather than from a path. Paths can be renamed or reached through a
// symlink; device/inode (or the equivalent platform file index) lets us reject
// a swapped root when it is revalidated.
type fileIdentity struct {
	device uint64
	inode  uint64
	volume uint64
	index  uint64
	valid  bool
}

func identityFor(info os.FileInfo) (fileIdentity, error) {
	if info == nil || info.Sys() == nil {
		return fileIdentity{}, ErrUnstableIdentity
	}
	value := reflect.ValueOf(info.Sys())
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return fileIdentity{}, ErrUnstableIdentity
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return fileIdentity{}, ErrUnstableIdentity
	}
	identity := fileIdentity{}
	identity.device, _ = uintField(value, "Dev")
	identity.inode, _ = uintField(value, "Ino")
	identity.volume, _ = uintField(value, "VolumeSerialNumber")
	high, highOK := uintField(value, "FileIndexHigh")
	low, lowOK := uintField(value, "FileIndexLow")
	if highOK || lowOK {
		identity.index = high<<32 | low
	}
	identity.valid = identity.device != 0 || identity.inode != 0 || identity.volume != 0 || identity.index != 0
	if !identity.valid {
		return fileIdentity{}, ErrUnstableIdentity
	}
	return identity, nil
}

func uintField(value reflect.Value, name string) (uint64, bool) {
	field := value.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint(), true
	default:
		return 0, false
	}
}

func sameIdentity(want, got fileIdentity) bool {
	return want.valid && got.valid && want == got
}

func fingerprintFor(canonicalPath string, info os.FileInfo, identity fileIdentity) string {
	value := fmt.Sprintf("agentbridge-repository-v1|%s|%d|%d|%d|%d", info.Mode().Type(), identity.device, identity.inode, identity.volume, identity.index)
	if identity.device == 0 && identity.inode == 0 && identity.volume == 0 && identity.index == 0 {
		value += "|" + strings.TrimSpace(canonicalPath)
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// Fingerprint returns a stable, opaque identity for a repository directory.
// The returned value does not contain the local path.
func Fingerprint(path string) (string, error) {
	canonical, err := os.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("fingerprint repository root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat repository root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("repository: root is not a directory")
	}
	identity, err := identityFor(info)
	if err != nil {
		return "", err
	}
	return fingerprintFor(canonical, info, identity), nil
}
