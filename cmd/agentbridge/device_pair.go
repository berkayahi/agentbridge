package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

// devicePairProofOutput keeps the typed proof fields alongside operator-facing
// provenance. Desktop extracts only PairDeviceRequest's fields before sending
// the authenticated mutation; the local API therefore remains strict about
// unknown JSON fields.
type devicePairProofOutput struct {
	Version                 int                     `json:"version"`
	DeviceID                string                  `json:"device_id"`
	ChallengeID             string                  `json:"challenge_id"`
	Name                    string                  `json:"name"`
	Kind                    localcontrol.DeviceKind `json:"kind"`
	Endpoint                string                  `json:"endpoint"`
	PublicKey               []byte                  `json:"public_key"`
	Signature               []byte                  `json:"signature"`
	IdempotencyKey          string                  `json:"idempotency_key"`
	Fingerprint             string                  `json:"fingerprint"`
	ControllerPublicKeyPath string                  `json:"controller_public_key_path"`
	ExpiresAt               time.Time               `json:"expires_at"`
}

func runDevicePairCommand(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pair device", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data-dir", strings.TrimSpace(os.Getenv("AGENTBRIDGE_DATA_DIR")), "device data directory")
	identityPath := flags.String("identity-path", "", "owner-only device key path")
	controllerPublicKeyPath := flags.String("controller-public-key-path", "", "owner-only controller public key path")
	challengePath := flags.String("challenge", "", "one-time local pairing challenge JSON path")
	name := flags.String("name", "", "display name for the paired device")
	endpoint := flags.String("endpoint", "", "device-agent WSS endpoint")
	outputPath := flags.String("output", "", "write pair request JSON to an owner-only file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*dataDir) == "" || !filepath.IsAbs(*dataDir) {
		fmt.Fprintln(stderr, "agentbridge: device pairing requires an absolute data directory")
		return 1
	}
	if err := privateDirectory(filepath.Clean(*dataDir)); err != nil {
		fmt.Fprintln(stderr, "agentbridge: device pairing data directory is unavailable")
		return 1
	}
	if strings.TrimSpace(*challengePath) == "" || !filepath.IsAbs(*challengePath) {
		fmt.Fprintln(stderr, "agentbridge: device pairing requires an absolute challenge path")
		return 1
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*name) != *name || len(*name) > 120 || strings.ContainsAny(*name, "\x00\r\n") {
		fmt.Fprintln(stderr, "agentbridge: device pairing name is invalid")
		return 1
	}
	if !validDevicePairEndpoint(*endpoint) {
		fmt.Fprintln(stderr, "agentbridge: device pairing endpoint must be a wss URL without user info or fragments")
		return 1
	}
	if strings.TrimSpace(*identityPath) == "" {
		*identityPath = filepath.Join(*dataDir, "device-key.json")
	}
	if strings.TrimSpace(*controllerPublicKeyPath) == "" {
		*controllerPublicKeyPath = filepath.Join(*dataDir, "controller-public-key.bin")
	}
	for label, path := range map[string]string{"identity": *identityPath, "controller public key": *controllerPublicKeyPath} {
		if !filepath.IsAbs(path) {
			fmt.Fprintf(stderr, "agentbridge: device pairing %s path must be absolute\n", label)
			return 1
		}
	}

	challenge, err := loadLocalPairingChallenge(*challengePath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: invalid local pairing challenge")
		return 1
	}
	now := time.Now().UTC()
	claim := deviceidentity.Claim{
		ID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID,
		BrowserFingerprint: challenge.BrowserFingerprint, ExpiresAt: challenge.ExpiresAt,
	}
	proofChallenge := deviceidentity.Challenge{
		ClaimID: challenge.ID, OrganizationID: "local", DeviceID: challenge.DeviceID,
		Nonce: challenge.Nonce, TrustSetDigest: challenge.TrustSetDigest, ExpiresAt: challenge.ExpiresAt,
	}
	if err := claim.Validate(now); err != nil {
		fmt.Fprintln(stderr, "agentbridge: local pairing challenge is expired or incomplete")
		return 1
	}
	if err := proofChallenge.Validate(now); err != nil || len(challenge.ControllerPublicKey) != 32 {
		fmt.Fprintln(stderr, "agentbridge: local pairing challenge has no valid controller key")
		return 1
	}
	key, err := loadOrCreateDeviceKey(*identityPath)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: device identity unavailable")
		return 1
	}
	proof, err := key.Prove(claim, proofChallenge, now)
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: device pairing proof failed")
		return 1
	}
	if err := persistControllerPublicKey(*controllerPublicKeyPath, challenge.ControllerPublicKey); err != nil {
		fmt.Fprintln(stderr, "agentbridge: controller public key could not be pinned")
		return 1
	}
	idempotencyKey, err := devicePairIdempotencyKey()
	if err != nil {
		fmt.Fprintln(stderr, "agentbridge: device pairing request ID unavailable")
		return 1
	}
	result := devicePairProofOutput{
		Version: 1, DeviceID: challenge.DeviceID, ChallengeID: challenge.ID,
		Name: *name, Kind: localcontrol.DeviceKindRaspberryPi, Endpoint: *endpoint,
		PublicKey: proof.PublicKey, Signature: proof.Signature, IdempotencyKey: idempotencyKey,
		Fingerprint: key.Fingerprint(), ControllerPublicKeyPath: *controllerPublicKeyPath,
		ExpiresAt: proof.ExpiresAt,
	}
	if err := writeEnrollmentOutput(*outputPath, result, stdout); err != nil {
		fmt.Fprintln(stderr, "agentbridge: unable to write device pairing request")
		return 1
	}
	return 0
}

func loadLocalPairingChallenge(path string) (localcontrol.PairingChallenge, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return localcontrol.PairingChallenge{}, errors.New("challenge must be an owner-only regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return localcontrol.PairingChallenge{}, err
	}
	defer file.Close()
	var value localcontrol.PairingChallenge
	decoder := json.NewDecoder(io.LimitReader(file, 16*1024))
	if err := decoder.Decode(&value); err != nil {
		return localcontrol.PairingChallenge{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return localcontrol.PairingChallenge{}, errors.New("challenge must contain exactly one JSON value")
	}
	if value.ID == "" || value.DeviceID == "" || value.BrowserFingerprint == "" || value.Nonce == "" || value.TrustSetDigest == "" || value.ExpiresAt.IsZero() {
		return localcontrol.PairingChallenge{}, errors.New("challenge fields are incomplete")
	}
	return value, nil
}

func validDevicePairEndpoint(value string) bool {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(value))
	return err == nil && parsed.Scheme == "wss" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && !strings.Contains(value, "#")
}

func persistControllerPublicKey(path string, value []byte) error {
	if !filepath.IsAbs(path) || len(value) != 32 {
		return errors.New("invalid controller public key path or value")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("controller public key file is unsafe")
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil || string(contents) != string(value) {
			return errors.New("controller public key changed")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func devicePairIdempotencyKey() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return "pair-device-" + hex.EncodeToString(value), nil
}
