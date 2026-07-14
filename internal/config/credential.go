package config

import (
	"errors"
	"fmt"
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
	contents, err := os.ReadFile(filepath.Join(dir, name))
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
