package config

import (
	"errors"
	"os"
)

var forbiddenAPIKeyEnvironment = []string{
	"OPENAI_API_KEY",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
}

func RejectAPIKeyEnvironment() error {
	for _, name := range forbiddenAPIKeyEnvironment {
		if os.Getenv(name) != "" {
			return errors.New("provider API-key environment variables are not supported")
		}
	}
	return nil
}
