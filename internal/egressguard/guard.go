// Package egressguard bounds and filters data before it leaves the device.
package egressguard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/berkayahi/agentbridge/internal/security"
)

type DataClass string

const (
	ClassStructuredMessage DataClass = "structured_message"
	ClassTerminalOutput    DataClass = "terminal_output"
	ClassArtifact          DataClass = "artifact_declaration"
	ClassCloudEvent        DataClass = "cloud_event"
)

var ErrQuarantined = errors.New("egress guard: data quarantined")

type Finding struct {
	Class  string `json:"class"`
	Reason string `json:"reason"`
}

type Event struct {
	DataClass   DataClass
	Digest      string
	Findings    []Finding
	Quarantined bool
}

type Config struct {
	MaxBytes    int
	MaxFindings int
	SecretPaths []string
	Secrets     []string
	OnFinding   func(Event)
}

type Result struct {
	Data        []byte
	Findings    []Finding
	Quarantined bool
}

type Guard struct {
	maxBytes    int
	maxFindings int
	secretPaths []string
	redactor    *security.Redactor
	onFinding   func(Event)
}

var (
	privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	credentialPattern = regexp.MustCompile(`(?i)(?:authorization\s*[:=]|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret)\s*[:=]|(?:gh[pousr]_\w{20,}|github_pat_\w{20,}))`)
	localPathPattern  = regexp.MustCompile(`(?:/(?:Users|home|private/var|tmp)/[^\s"']+|[A-Za-z]:\\Users\\[^\s"']+)`)
	longTokenPattern  = regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
)

func New(cfg Config) *Guard {
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 128 << 10
	}
	maxFindings := cfg.MaxFindings
	if maxFindings <= 0 {
		maxFindings = 16
	}
	paths := make([]string, 0, len(cfg.SecretPaths))
	for _, path := range cfg.SecretPaths {
		if filepath.IsAbs(path) {
			paths = append(paths, filepath.Clean(path))
		}
	}
	return &Guard{maxBytes: maxBytes, maxFindings: maxFindings, secretPaths: paths, redactor: security.NewRedactor(security.Config{Secrets: cfg.Secrets, MaxPayloadRunes: maxBytes}), onFinding: cfg.OnFinding}
}

func (g *Guard) Inspect(class DataClass, payload []byte) Result {
	if g == nil {
		return Result{Data: append([]byte(nil), payload...)}
	}
	bounded := append([]byte(nil), payload...)
	findings := make([]Finding, 0, 4)
	add := func(className, reason string) {
		if len(findings) < g.maxFindings {
			findings = append(findings, Finding{Class: className, Reason: reason})
		}
	}
	if len(bounded) > g.maxBytes {
		bounded = bounded[:g.maxBytes]
		add("payload", "size_bound")
	}
	text := string(bounded)
	if privateKeyPattern.MatchString(text) || credentialPattern.MatchString(text) {
		add("credential", "credential_pattern")
	}
	if localPathPattern.MatchString(text) {
		add("local_path", "local_path")
	}
	for _, path := range g.secretPaths {
		if strings.Contains(text, path) {
			add("local_secret_path", "configured_secret_path")
		}
	}
	for _, token := range longTokenPattern.FindAllString(text, g.maxFindings) {
		if entropy(token) >= 4.25 {
			add("entropy", "high_entropy_token")
			break
		}
	}
	redacted := g.redactor.RedactBytes(bounded)
	for _, path := range g.secretPaths {
		redacted = []byte(strings.ReplaceAll(string(redacted), path, "[REDACTED:local-path]"))
	}
	return Result{Data: redacted, Findings: findings, Quarantined: len(findings) > 0}
}

func (g *Guard) Check(class DataClass, payload []byte) ([]byte, error) {
	result := g.Inspect(class, payload)
	if !result.Quarantined {
		return result.Data, nil
	}
	digest := sha256.Sum256(payload)
	event := Event{DataClass: class, Digest: hex.EncodeToString(digest[:]), Findings: append([]Finding(nil), result.Findings...), Quarantined: true}
	if g != nil && g.onFinding != nil {
		g.onFinding(event)
	}
	return result.Data, fmt.Errorf("%w: %s", ErrQuarantined, event.Digest)
}

func entropy(value string) float64 {
	if value == "" {
		return 0
	}
	var counts [256]int
	for _, runeValue := range value {
		if runeValue > unicode.MaxASCII {
			continue
		}
		counts[runeValue]++
	}
	length := len(value)
	if length == 0 {
		return 0
	}
	result := 0.0
	for _, count := range counts {
		if count == 0 {
			continue
		}
		probability := float64(count) / float64(length)
		result -= probability * log2(probability)
	}
	return result
}

func log2(value float64) float64 {
	// Avoid importing math for this tiny bounded calculation.
	result := 0.0
	for value < 1 {
		value *= 2
		result--
	}
	for value >= 2 {
		value /= 2
		result++
	}
	return result
}
