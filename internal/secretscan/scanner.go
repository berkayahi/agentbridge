// Package secretscan provides a bounded, fail-closed publication scanner.
//
// The scanner is deliberately independent of Git and process execution. Callers
// choose the exact staged paths or artifacts to scan; an error is never treated
// as a warning.
package secretscan

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	ErrFinding      = errors.New("secretscan: secret finding")
	ErrTooLarge     = errors.New("secretscan: input exceeds scan bound")
	ErrInvalidInput = errors.New("secretscan: invalid input")
)

const (
	defaultMaxFileBytes  int64 = 32 << 20
	defaultMaxTotalBytes int64 = 64 << 20
	defaultTimeout             = 2 * time.Second
)

// Input names the exact local files and in-memory artifacts to scan.
type Input struct {
	Root      string
	Paths     []string
	Artifacts []Artifact
}

type Artifact struct {
	Name string
	Data []byte
}

type Config struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
	Timeout       time.Duration
	Allowlist     []string
}

type Finding struct {
	RuleID string
	Path   string
}

type Report struct {
	Findings []Finding
	Files    int
	Bytes    int64
	Digest   string
}

// Scanner is the publication-gate contract used by the Git broker.
type Scanner interface {
	Scan(context.Context, Input) (Report, error)
}

// Detector is a bounded detector profile. It is intentionally conservative:
// false positives can be explicitly allowlisted, while scanner failures block
// publication.
type Detector struct {
	maxFileBytes  int64
	maxTotalBytes int64
	timeout       time.Duration
	allowlist     []string
	patterns      []secretPattern
}

type secretPattern struct {
	id      string
	pattern *regexp.Regexp
}

func NewDetector(cfg Config) Detector {
	maxFile := cfg.MaxFileBytes
	if maxFile <= 0 {
		maxFile = defaultMaxFileBytes
	}
	maxTotal := cfg.MaxTotalBytes
	if maxTotal <= 0 {
		maxTotal = defaultMaxTotalBytes
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	patterns := []secretPattern{
		{id: "private-key", pattern: regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
		{id: "github-token", pattern: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)},
		{id: "openai-token", pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
		{id: "aws-access-key", pattern: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
		{id: "telegram-token", pattern: regexp.MustCompile(`\b[0-9]{8,12}:[A-Za-z0-9_-]{30,}\b`)},
		{id: "credential-assignment", pattern: regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|password|secret)\b\s*[:=]\s*["']?[A-Za-z0-9_./+=:-]{16,}`)},
	}
	allowlist := make([]string, 0, len(cfg.Allowlist))
	for _, value := range cfg.Allowlist {
		if strings.TrimSpace(value) != "" {
			allowlist = append(allowlist, value)
		}
	}
	return Detector{maxFileBytes: maxFile, maxTotalBytes: maxTotal, timeout: timeout, allowlist: allowlist, patterns: patterns}
}

func (d Detector) Scan(parent context.Context, input Input) (Report, error) {
	if parent == nil {
		return Report{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(parent, d.timeout)
	defer cancel()

	if input.Root == "" && len(input.Paths) > 0 {
		return Report{}, ErrInvalidInput
	}
	root := input.Root
	if root != "" {
		if !filepath.IsAbs(root) {
			return Report{}, ErrInvalidInput
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return Report{}, fmt.Errorf("%w: scan root is unavailable", ErrInvalidInput)
		}
	}

	paths := input.Paths
	if root != "" && len(paths) == 0 {
		var walked []string
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if path != root && entry.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			walked = append(walked, path)
			return nil
		})
		if err != nil {
			return Report{}, fmt.Errorf("walk scan root: %w", err)
		}
		paths = walked
	}

	report := Report{}
	hash := sha256.New()
	seen := make(map[string]struct{}, len(paths))
	for _, rawPath := range paths {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		path, display, err := containedPath(root, rawPath)
		if err != nil {
			return report, err
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		content, err := d.readFile(ctx, path, &report)
		if err != nil {
			return report, err
		}
		report.Files++
		report.Bytes += int64(len(content))
		_, _ = hash.Write([]byte(display))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
		if finding, ok := d.find(display, content); ok {
			report.Findings = append(report.Findings, finding)
		}
	}
	for _, artifact := range input.Artifacts {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if strings.TrimSpace(artifact.Name) == "" || strings.ContainsRune(artifact.Name, '\x00') {
			return report, ErrInvalidInput
		}
		content := append([]byte(nil), artifact.Data...)
		if err := d.account(int64(len(content)), &report); err != nil {
			return report, err
		}
		report.Files++
		report.Bytes += int64(len(content))
		_, _ = hash.Write([]byte(artifact.Name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(content)
		if finding, ok := d.find(artifact.Name, content); ok {
			report.Findings = append(report.Findings, finding)
		}
	}
	report.Digest = fmt.Sprintf("%x", hash.Sum(nil))
	if len(report.Findings) > 0 {
		return report, fmt.Errorf("%w: %s", ErrFinding, report.Findings[0].RuleID)
	}
	return report, nil
}

func (d Detector) ScanBytes(parent context.Context, name string, data []byte) (Report, error) {
	return d.Scan(parent, Input{Artifacts: []Artifact{{Name: name, Data: data}}})
}

func (d Detector) readFile(ctx context.Context, path string, report *Report) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: non-regular scan input", ErrInvalidInput)
	}
	if info.Size() > d.maxFileBytes {
		return nil, ErrTooLarge
	}
	if err := d.account(info.Size(), report); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, d.maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > d.maxFileBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (d Detector) account(size int64, report *Report) error {
	if size < 0 || size > d.maxFileBytes || report.Bytes > d.maxTotalBytes-size {
		return ErrTooLarge
	}
	return nil
}

func (d Detector) find(path string, content []byte) (Finding, bool) {
	text := string(content)
	for _, value := range d.patterns {
		if !value.pattern.MatchString(text) || d.allowed(path, value.id, text) {
			continue
		}
		return Finding{RuleID: value.id, Path: path}, true
	}
	// Detect a credential that was base64 encoded before it reached the
	// publication boundary. Only candidates with a meaningful decoded payload
	// are considered, keeping arbitrary binary files bounded and quiet.
	base64Candidate := regexp.MustCompile(`[A-Za-z0-9+/=_-]{32,}`)
	for _, candidate := range base64Candidate.FindAllString(text, 64) {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimRight(candidate, "="))
		if err != nil || len(decoded) == 0 {
			continue
		}
		for _, value := range d.patterns {
			if value.pattern.Match(decoded) && !d.allowed(path, "encoded-"+value.id, text) {
				return Finding{RuleID: "encoded-" + value.id, Path: path}, true
			}
		}
	}
	return Finding{}, false
}

func (d Detector) allowed(path, rule, content string) bool {
	for _, value := range d.allowlist {
		if value == path || value == rule || strings.Contains(content, value) {
			return true
		}
	}
	return false
}

func containedPath(root, raw string) (string, string, error) {
	if strings.TrimSpace(raw) == "" || strings.ContainsRune(raw, '\x00') {
		return "", "", ErrInvalidInput
	}
	path := raw
	if root != "" && !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if root != "" {
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", "", ErrInvalidInput
		}
		return filepath.Clean(path), filepath.ToSlash(rel), nil
	}
	if !filepath.IsAbs(path) {
		return "", "", ErrInvalidInput
	}
	return filepath.Clean(path), filepath.Base(path), nil
}

var _ Scanner = Detector{}
