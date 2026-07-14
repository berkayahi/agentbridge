package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const redactedCredential = "[REDACTED]"

type Credential struct {
	value string
}

func (c Credential) Value() string {
	return c.value
}

func (Credential) String() string {
	return redactedCredential
}

func (Credential) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(redactedCredential))
}

type CredentialReader struct{}

func (CredentialReader) Read(name string) (Credential, error) {
	if !validCredentialFilename(name) {
		return Credential{}, errors.New("invalid credential filename")
	}
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if !filepath.IsAbs(dir) {
		return Credential{}, errors.New("credentials directory is not configured")
	}

	// systemd creates CREDENTIALS_DIRECTORY as a trusted, service-private
	// directory. Validate the leaf before and after opening so it cannot be a
	// symlink or be swapped between validation and use.
	path := filepath.Join(dir, name)
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return Credential{}, errors.New("credential is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return Credential{}, errors.New("credential is unavailable")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return Credential{}, errors.New("credential is unavailable")
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return Credential{}, errors.New("credential is unavailable")
	}
	value := strings.TrimSpace(string(contents))
	if value == "" {
		return Credential{}, errors.New("credential is empty")
	}
	return Credential{value: value}, nil
}

func validCredentialFilename(name string) bool {
	return name != "." && name != ".." && namePattern.MatchString(name)
}
