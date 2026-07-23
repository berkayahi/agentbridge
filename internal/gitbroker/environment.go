package gitbroker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrInvalidEnvironment = errors.New("gitbroker: invalid process environment")

// EnvironmentConfig constructs a fresh allowlisted environment. It never
// copies the caller's environment, so provider credentials and SSH agent state
// cannot reach Git or the hosting CLI accidentally.
type EnvironmentConfig struct {
	Home    string
	TempDir string
	Path    string
}

func (e EnvironmentConfig) Base() ([]string, error) {
	home := e.Home
	if home == "" {
		home = filepath.Join(os.TempDir(), "agentbridge-no-home")
	}
	if !filepath.IsAbs(home) || strings.ContainsAny(home, "\x00\r\n") {
		return nil, ErrInvalidEnvironment
	}
	temp := e.TempDir
	if temp == "" {
		temp = os.TempDir()
	}
	if !filepath.IsAbs(temp) {
		return nil, ErrInvalidEnvironment
	}
	path := e.Path
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	if strings.ContainsAny(path, "\x00\r\n") {
		return nil, ErrInvalidEnvironment
	}
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"PATH=" + path,
		"TMPDIR=" + temp,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_PROTOCOL_FROM_USER=0",
		"GH_PAGER=cat",
	}, nil
}

func (e EnvironmentConfig) WithCredential(credential Credential) ([]string, func(), error) {
	base, err := e.Base()
	if err != nil {
		return nil, func() {}, err
	}
	if credential.value == "" {
		return base, func() {}, nil
	}
	dir, err := os.MkdirTemp(e.TempDir, "agentbridge-askpass-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("create credential boundary: %w", err)
	}
	path := filepath.Join(dir, "askpass")
	const askpass = "#!/bin/sh\ncase \"$1\" in\n  *Username*) printf '%s' \"$AGENTBRIDGE_GIT_USERNAME\" ;;\n  *) printf '%s' \"$AGENTBRIDGE_GIT_SECRET\" ;;\nesac\n"
	if err := os.WriteFile(path, []byte(askpass), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, func() {}, fmt.Errorf("create credential helper: %w", err)
	}
	base = appendEnv(base, "GIT_ASKPASS="+path, "SSH_ASKPASS="+path, "AGENTBRIDGE_GIT_USERNAME="+credential.username, "AGENTBRIDGE_GIT_SECRET="+credential.value)
	return base, func() { _ = os.RemoveAll(dir) }, nil
}

func (e EnvironmentConfig) WithCommandCredential(credential Credential) ([]string, func(), error) {
	base, err := e.Base()
	if err != nil {
		return nil, func() {}, err
	}
	if credential.value == "" {
		return base, func() {}, nil
	}
	return appendEnv(base, "GH_TOKEN="+credential.value), func() {}, nil
}

func appendEnv(base []string, additions ...string) []string {
	result := append([]string(nil), base...)
	for _, addition := range additions {
		key, _, _ := strings.Cut(addition, "=")
		for i, existing := range result {
			if existingKey, _, _ := strings.Cut(existing, "="); existingKey == key {
				result[i] = addition
				goto next
			}
		}
		result = append(result, addition)
	next:
	}
	return result
}
