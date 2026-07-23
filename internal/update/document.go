package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrUntrustedDocument = errors.New("update: untrusted local document")

type metadataDocument struct {
	Version    uint64            `json:"version"`
	ExpiresAt  json.RawMessage   `json:"expires_at"`
	Identity   BinaryIdentity    `json:"identity"`
	SignerIDs  []string          `json:"signer_ids"`
	Signatures map[string]string `json:"signatures"`
}

type trustRootDocument struct {
	Threshold int               `json:"threshold"`
	Keys      map[string]string `json:"keys"`
}

// ReadMetadataFile reads a signed metadata envelope from a local protected
// file. Network locations and writable-by-group/other files are deliberately
// outside this API; managed commands receive an already verified version
// choice, never a caller-provided URL or trust root.
func ReadMetadataFile(path string) (Metadata, error) {
	content, err := readProtectedDocument(path, 1<<20)
	if err != nil {
		return Metadata{}, err
	}
	var document metadataDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return Metadata{}, ErrInvalidMetadata
	}
	expiresAt, err := parseDocumentTime(document.ExpiresAt)
	if err != nil {
		return Metadata{}, ErrInvalidMetadata
	}
	signatures := make(map[string][]byte, len(document.Signatures))
	for id, encoded := range document.Signatures {
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(value) != ed25519.SignatureSize {
			return Metadata{}, ErrInvalidMetadata
		}
		signatures[id] = value
	}
	return Metadata{Version: document.Version, ExpiresAt: expiresAt, Identity: document.Identity, SignerIDs: document.SignerIDs, Signatures: signatures}, nil
}

// ReadTrustRootFile loads only public Ed25519 keys from a protected local
// document. The update root is a separate trust domain from managed command
// signing keys and is never inferred from device enrollment state.
func ReadTrustRootFile(path string) (TrustRoot, error) {
	content, err := readProtectedDocument(path, 64<<10)
	if err != nil {
		return TrustRoot{}, err
	}
	var document trustRootDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return TrustRoot{}, ErrTrust
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for id, encoded := range document.Keys {
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(value) != ed25519.PublicKeySize {
			return TrustRoot{}, ErrTrust
		}
		keys[id] = ed25519.PublicKey(append([]byte(nil), value...))
	}
	root := TrustRoot{Keys: keys, Threshold: document.Threshold}
	if err := root.Validate(); err != nil {
		return TrustRoot{}, err
	}
	return root, nil
}

func readProtectedDocument(path string, limit int64) ([]byte, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrUntrustedDocument
	}
	cleanPath := filepath.Clean(path)
	info, err := os.Lstat(cleanPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, ErrUntrustedDocument
	}
	file, err := os.Open(cleanPath)
	if err != nil {
		return nil, ErrUntrustedDocument
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, ErrUntrustedDocument
	}
	return content, nil
}

func parseDocumentTime(raw json.RawMessage) (time.Time, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return time.Parse(time.RFC3339Nano, text)
	}
	var nanos int64
	if err := json.Unmarshal(raw, &nanos); err == nil && nanos > 0 {
		return time.Unix(0, nanos).UTC(), nil
	}
	return time.Time{}, ErrInvalidMetadata
}
