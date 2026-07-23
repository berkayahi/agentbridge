package gitbroker

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var ErrInvalidRepository = errors.New("gitbroker: invalid repository registration")

// RepositoryConfig is the only source of local paths and remote names used by
// the broker. Operation messages carry only opaque IDs.
type RepositoryConfig struct {
	ID            string
	CheckoutPath  string
	WorktreeRoot  string
	RemoteName    string
	RemoteURL     string
	CredentialRef string
}

func (r RepositoryConfig) Validate() error {
	if !validID(r.ID) || !filepath.IsAbs(r.CheckoutPath) || !filepath.IsAbs(r.WorktreeRoot) ||
		!safeRemoteName(r.RemoteName) || strings.TrimSpace(r.RemoteURL) == "" {
		return ErrInvalidRepository
	}
	if err := validateRemoteURL(r.RemoteURL); err != nil {
		return err
	}
	if r.CredentialRef != "" && !validID(r.CredentialRef) {
		return ErrInvalidRepository
	}
	return nil
}

type Registry struct{ repositories map[string]RepositoryConfig }

func NewRegistry(values ...RepositoryConfig) (*Registry, error) {
	result := &Registry{repositories: make(map[string]RepositoryConfig, len(values))}
	for _, value := range values {
		if err := value.Validate(); err != nil {
			return nil, err
		}
		if _, exists := result.repositories[value.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate repository %q", ErrInvalidRepository, value.ID)
		}
		result.repositories[value.ID] = value
	}
	if len(result.repositories) == 0 {
		return nil, ErrInvalidRepository
	}
	return result, nil
}

func (r *Registry) Resolve(id string) (RepositoryConfig, error) {
	if r == nil {
		return RepositoryConfig{}, ErrInvalidRepository
	}
	value, ok := r.repositories[id]
	if !ok {
		return RepositoryConfig{}, ErrUnregisteredRemote
	}
	return value, nil
}

func (r *Registry) List() []RepositoryConfig {
	if r == nil {
		return nil
	}
	result := make([]RepositoryConfig, 0, len(r.repositories))
	for _, value := range r.repositories {
		result = append(result, value)
	}
	return result
}

func safeRemoteName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return value != "." && value != ".."
}

func validateRemoteURL(raw string) error {
	if strings.ContainsAny(raw, "\x00\r\n") || strings.Contains(raw, "ext::") || strings.Contains(raw, "!") {
		return ErrInvalidRepository
	}
	if strings.HasPrefix(raw, "git@") {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 || strings.Contains(parts[0], "/") || strings.Trim(parts[1], "/") == "" {
			return ErrInvalidRepository
		}
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrInvalidRepository
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "ssh", "git", "file":
		return nil
	default:
		return ErrInvalidRepository
	}
}

func normalizeRemoteURL(raw string) string {
	if strings.HasPrefix(raw, "git@") {
		return strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = filepath.ToSlash(strings.TrimSuffix(parsed.Path, ".git"))
	return parsed.String()
}

func sameRemoteURL(expected, actual string) bool {
	if normalizeRemoteURL(expected) == normalizeRemoteURL(actual) {
		return true
	}
	if !strings.Contains(expected, "://") && !strings.HasPrefix(expected, "git@") {
		expectedPath, errExpected := filepath.Abs(expected)
		actualPath, errActual := filepath.Abs(actual)
		return errExpected == nil && errActual == nil && filepath.Clean(expectedPath) == filepath.Clean(actualPath)
	}
	return false
}

func pathExistsAsDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrInvalidRepository
	}
	return nil
}
