// Package config loads and validates Agent Bridge configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/goccy/go-yaml"
)

type Config struct {
	DefaultRepository string                       `yaml:"default_repository,omitempty"`
	Server            ServerConfig                 `yaml:"server"`
	Telegram          TelegramConfig               `yaml:"telegram"`
	Providers         map[string]ProviderConfig    `yaml:"providers"`
	Repositories      map[string]RepositoryProfile `yaml:"repositories"`
}

type ServerConfig struct {
	Listen                     string   `yaml:"listen"`
	AllowedTailscaleIdentities []string `yaml:"allowed_tailscale_identities"`
}

type TelegramConfig struct {
	PrivateChatOnly bool    `yaml:"private_chat_only"`
	AllowedUserIDs  []int64 `yaml:"allowed_user_ids"`
	PairedChatID    int64   `yaml:"paired_chat_id"`
	ForceIPv4       bool    `yaml:"force_ipv4"`
}

type ProviderConfig struct {
	Executable string `yaml:"executable"`
	Model      string `yaml:"model"`
}

type RepositoryProfile struct {
	CheckoutPath  string                `yaml:"checkout_path"`
	Remote        string                `yaml:"remote"`
	BaseRef       string                `yaml:"base_ref"`
	Verification  []VerificationCommand `yaml:"verification"`
	DeploymentURL string                `yaml:"deployment_url,omitempty"`
	Delivery      DeliveryPolicy        `yaml:"delivery,omitempty"`
}

type VerificationCommand struct {
	Argv []string `yaml:"argv"`
	Dir  string   `yaml:"dir,omitempty"`
}

type DeliveryPolicy struct {
	Enabled    bool   `yaml:"enabled"`
	AllowedRef string `yaml:"allowed_ref,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var modelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
var codexModelPattern = regexp.MustCompile(`^gpt-([0-9]+)\.([0-9]+)-(terra|sol)(?:[.-][a-zA-Z0-9._-]+)?$`)

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()

	var cfg Config
	decoder := yaml.NewDecoder(f, yaml.DisallowUnknownField())
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if err := rejectTrailingYAML(decoder); err != nil {
		return Config{}, err
	}
	if cfg.DefaultRepository == "" && len(cfg.Repositories) == 1 {
		for name := range cfg.Repositories {
			cfg.DefaultRepository = name
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func rejectTrailingYAML(decoder *yaml.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return errors.New("decode configuration: multiple YAML documents are not allowed")
}
