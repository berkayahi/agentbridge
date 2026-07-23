package deviceidentity

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrInvalidRecord = errors.New("device identity: invalid enrollment record")

// EnrollmentRecord contains only public identity and trust facts. It never
// contains the private device key or provider/Git credentials.
type EnrollmentRecord struct {
	Version                int    `json:"version"`
	ClaimID                string `json:"claim_id"`
	OrganizationID         string `json:"organization_id"`
	DeviceID               string `json:"device_id"`
	Fingerprint            string `json:"fingerprint"`
	BrowserFingerprint     string `json:"browser_fingerprint,omitempty"`
	TrustSetDigest         string `json:"trust_set_digest"`
	HighestControllerEpoch uint64 `json:"highest_controller_epoch"`
	Mode                   string `json:"mode"`
	Revoked                bool   `json:"revoked,omitempty"`
	Quarantined            bool   `json:"quarantined,omitempty"`
}

func (r EnrollmentRecord) Validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.Version != 1 || !valid(r.ClaimID) || !valid(r.OrganizationID) || !valid(r.DeviceID) || !valid(r.Fingerprint) || !valid(r.TrustSetDigest) || r.HighestControllerEpoch == 0 || r.Mode != "managed" {
		return ErrInvalidRecord
	}
	if len(r.Fingerprint) != 64 || strings.ContainsAny(r.Fingerprint, " \t\r\n") {
		return ErrInvalidRecord
	}
	if _, err := hex.DecodeString(r.Fingerprint); err != nil {
		return ErrInvalidRecord
	}
	return nil
}

func SaveRecord(path string, record EnrollmentRecord) error {
	if record.Version == 0 {
		record.Version = 1
	}
	if err := record.Validate(context.Background()); err != nil {
		return err
	}
	return writeRecord(path, record)
}

func LoadRecord(path string) (EnrollmentRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return EnrollmentRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return EnrollmentRecord{}, ErrInvalidRecord
	}
	file, err := os.Open(path)
	if err != nil {
		return EnrollmentRecord{}, fmt.Errorf("open enrollment record: %w", err)
	}
	defer file.Close()
	var record EnrollmentRecord
	if err := json.NewDecoder(io.LimitReader(file, 16*1024)).Decode(&record); err != nil {
		return EnrollmentRecord{}, fmt.Errorf("decode enrollment record: %w", err)
	}
	if err := record.Validate(context.Background()); err != nil {
		return EnrollmentRecord{}, err
	}
	return record, nil
}

func writeRecord(path string, record EnrollmentRecord) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create enrollment directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("protect enrollment directory: %w", err)
	}
	value, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode enrollment record: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".enrollment-*")
	if err != nil {
		return fmt.Errorf("create enrollment record: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(value, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write enrollment record: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync enrollment record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install enrollment record: %w", err)
	}
	return nil
}
