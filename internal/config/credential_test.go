package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialReaderReadsAndTrimsCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if err := os.WriteFile(filepath.Join(dir, "telegram-token"), []byte(" secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	credential, err := (CredentialReader{}).Read("telegram-token")
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got := credential.Value(); got != "secret-token" {
		t.Fatalf("Value() = %q, want trimmed token", got)
	}
	for _, formatted := range []string{fmt.Sprint(credential), fmt.Sprintf("%q", credential), fmt.Sprintf("%#v", credential)} {
		if strings.Contains(formatted, "secret-token") {
			t.Fatalf("formatted credential leaks secret: %q", formatted)
		}
	}
}

func TestCredentialReaderRejectsTraversal(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	for _, name := range []string{"../token", "sub/token", ".", ""} {
		t.Run(name, func(t *testing.T) {
			if _, err := (CredentialReader{}).Read(name); err == nil {
				t.Fatal("Read() error = nil, want invalid filename error")
			}
		})
	}
}

func TestCredentialReaderRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-token")
	if err := os.WriteFile(outside, []byte("must-not-be-read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "telegram-token")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	credential, err := (CredentialReader{}).Read("telegram-token")
	if err == nil {
		t.Fatalf("Read() = %v, nil; want symlink rejection", credential)
	}
}

func TestCredentialReaderRejectsEmptyCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (CredentialReader{}).Read("empty"); err == nil {
		t.Fatal("Read() error = nil, want empty credential error")
	}
}
