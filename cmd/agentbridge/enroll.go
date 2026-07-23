package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
)

type enrollmentRequestOutput struct {
	Version            int       `json:"version"`
	ClaimID            string    `json:"claim_id"`
	OrganizationID     string    `json:"organization_id"`
	DeviceID           string    `json:"device_id"`
	PublicKey          string    `json:"device_public_key"`
	Fingerprint        string    `json:"fingerprint"`
	BrowserFingerprint string    `json:"browser_fingerprint"`
	ExpiresAt          time.Time `json:"expires_at"`
}

type enrollmentChallengeInput struct {
	ClaimID            string            `json:"claim_id"`
	OrganizationID     string            `json:"organization_id"`
	DeviceID           string            `json:"device_id"`
	Nonce              string            `json:"nonce"`
	TrustSetDigest     string            `json:"trust_set_digest"`
	CommandSigningKeys map[string][]byte `json:"command_signing_keys,omitempty"`
	ExpiresAt          time.Time         `json:"expires_at"`
}

type enrollmentProofOutput struct {
	Version            int               `json:"version"`
	ClaimID            string            `json:"claim_id"`
	OrganizationID     string            `json:"organization_id"`
	DeviceID           string            `json:"device_id"`
	Nonce              string            `json:"nonce"`
	PublicKey          string            `json:"device_public_key"`
	Fingerprint        string            `json:"fingerprint"`
	Signature          string            `json:"signature"`
	TrustSetDigest     string            `json:"trust_set_digest"`
	CommandSigningKeys map[string][]byte `json:"command_signing_keys,omitempty"`
	ExpiresAt          time.Time         `json:"expires_at"`
}

func runEnrollCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", strings.TrimSpace(os.Getenv("AGENTBRIDGE_DATA_DIR")), "managed data directory")
	identityPath := flags.String("identity-path", "", "owner-only device key path")
	recordPath := flags.String("record-path", "", "owner-only enrollment record path")
	claimID := flags.String("claim-id", "", "one-time platform claim ID")
	organizationID := flags.String("organization-id", "", "platform organization ID")
	deviceID := flags.String("device-id", "", "platform device ID")
	browserFingerprint := flags.String("browser-fingerprint", "", "browser-confirmed fingerprint")
	challengePath := flags.String("challenge", "", "platform challenge JSON path")
	outputPath := flags.String("output", "", "write request/proof JSON to an owner-only file")
	expiresIn := flags.Duration("expires-in", 10*time.Minute, "request validity duration")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*dataDir) == "" || !filepath.IsAbs(*dataDir) || *expiresIn <= 0 {
		fmt.Fprintln(stderr, "agentbridge: enrollment requires an absolute data directory and positive expiry")
		return 1
	}
	if strings.TrimSpace(*identityPath) == "" {
		*identityPath = filepath.Join(*dataDir, "device-key.json")
	}
	if strings.TrimSpace(*recordPath) == "" {
		*recordPath = filepath.Join(*dataDir, "enrollment.json")
	}
	if !filepath.IsAbs(*identityPath) || !filepath.IsAbs(*recordPath) {
		fmt.Fprintln(stderr, "agentbridge: enrollment paths must be absolute")
		return 1
	}
	claim := deviceidentity.Claim{
		ID: strings.TrimSpace(*claimID), OrganizationID: strings.TrimSpace(*organizationID), DeviceID: strings.TrimSpace(*deviceID),
		BrowserFingerprint: strings.TrimSpace(*browserFingerprint), ExpiresAt: time.Now().UTC().Add(*expiresIn),
	}
	if err := claim.Validate(time.Now().UTC()); err != nil {
		fmt.Fprintln(stderr, "agentbridge: enrollment claim is incomplete")
		return 1
	}
	key, err := loadOrCreateDeviceKey(*identityPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: device identity unavailable")
		return 1
	}
	if strings.TrimSpace(*challengePath) == "" {
		result := enrollmentRequestOutput{
			Version: 1, ClaimID: claim.ID, OrganizationID: claim.OrganizationID, DeviceID: claim.DeviceID,
			PublicKey: base64.StdEncoding.EncodeToString(key.PublicKey()), Fingerprint: key.Fingerprint(),
			BrowserFingerprint: claim.BrowserFingerprint, ExpiresAt: claim.ExpiresAt,
		}
		if err := writeEnrollmentOutput(*outputPath, result, stdout); err != nil {
			fmt.Fprintln(stderr, "agentbridge: unable to write enrollment request")
			return 1
		}
		return 0
	}
	challenge, err := loadEnrollmentChallenge(*challengePath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: invalid enrollment challenge")
		return 1
	}
	proof, err := key.Prove(claim, challenge, time.Now().UTC())
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: enrollment proof failed")
		return 1
	}
	record := deviceidentity.EnrollmentRecord{
		Version: 1, ClaimID: proof.ClaimID, OrganizationID: proof.OrganizationID, DeviceID: proof.DeviceID,
		Fingerprint: key.Fingerprint(), BrowserFingerprint: claim.BrowserFingerprint,
		TrustSetDigest: proof.TrustSetDigest, CommandSigningKeys: proof.CommandSigningKeys, HighestControllerEpoch: 1, Mode: "managed",
	}
	if err := deviceidentity.SaveRecord(*recordPath, record); err != nil {
		fmt.Fprintln(stderr, "agentbridge: unable to persist enrollment record")
		return 1
	}
	result := enrollmentProofOutput{
		Version: 1, ClaimID: proof.ClaimID, OrganizationID: proof.OrganizationID, DeviceID: proof.DeviceID,
		Nonce: proof.Nonce, PublicKey: base64.StdEncoding.EncodeToString(proof.PublicKey), Fingerprint: key.Fingerprint(),
		Signature: base64.StdEncoding.EncodeToString(proof.Signature), TrustSetDigest: proof.TrustSetDigest, CommandSigningKeys: proof.CommandSigningKeys, ExpiresAt: proof.ExpiresAt,
	}
	if err := writeEnrollmentOutput(*outputPath, result, stdout); err != nil {
		fmt.Fprintln(stderr, "agentbridge: unable to write enrollment proof")
		return 1
	}
	return 0
}

func loadOrCreateDeviceKey(path string) (deviceidentity.Key, error) {
	if _, err := os.Stat(path); err == nil {
		return deviceidentity.Load(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return deviceidentity.Key{}, err
	}
	key, err := deviceidentity.Generate()
	if err != nil {
		return deviceidentity.Key{}, err
	}
	if err := deviceidentity.Save(path, key); err != nil {
		return deviceidentity.Key{}, err
	}
	return key, nil
}

func loadEnrollmentChallenge(path string) (deviceidentity.Challenge, error) {
	file, err := os.Open(path)
	if err != nil {
		return deviceidentity.Challenge{}, err
	}
	defer file.Close()
	var input enrollmentChallengeInput
	if err := json.NewDecoder(io.LimitReader(file, 16*1024)).Decode(&input); err != nil {
		return deviceidentity.Challenge{}, err
	}
	challenge := deviceidentity.Challenge{ClaimID: strings.TrimSpace(input.ClaimID), OrganizationID: strings.TrimSpace(input.OrganizationID), DeviceID: strings.TrimSpace(input.DeviceID), Nonce: strings.TrimSpace(input.Nonce), TrustSetDigest: strings.TrimSpace(input.TrustSetDigest), CommandSigningKeys: input.CommandSigningKeys, ExpiresAt: input.ExpiresAt}
	if err := challenge.Validate(time.Now().UTC()); err != nil {
		return deviceidentity.Challenge{}, err
	}
	return challenge, nil
}

func writeEnrollmentOutput(path string, value any, stdout io.Writer) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if strings.TrimSpace(path) == "" {
		_, err = stdout.Write(data)
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
