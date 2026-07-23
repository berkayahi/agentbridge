package gitbroker

import (
	"context"
	"errors"
	"strings"
)

var ErrCredentialUnavailable = errors.New("gitbroker: credential unavailable")

type Credential struct {
	label    string
	username string
	value    string
}

func NewCredential(label, value string) (Credential, error) {
	return NewCredentialWithUsername(label, "x-access-token", value)
}

func NewCredentialWithUsername(label, username, value string) (Credential, error) {
	if !validID(label) || strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") || strings.ContainsAny(username, "\x00\r\n") {
		return Credential{}, ErrCredentialUnavailable
	}
	return Credential{label: label, username: username, value: value}, nil
}

func (c Credential) Label() string    { return c.label }
func (c Credential) Username() string { return c.username }
func (c Credential) Value() string    { return c.value }
func (Credential) String() string     { return "[REDACTED]" }

type CredentialSource interface {
	Get(context.Context, string) (Credential, error)
}

type StaticCredentialSource struct{ Values map[string]Credential }

func (s StaticCredentialSource) Get(ctx context.Context, reference string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	value, ok := s.Values[reference]
	if !ok || value.value == "" {
		return Credential{}, ErrCredentialUnavailable
	}
	return value, nil
}

var _ CredentialSource = StaticCredentialSource{}
