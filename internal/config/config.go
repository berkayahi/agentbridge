// Package config loads and validates Agent Bridge configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/berkayahi/agentbridge/internal/spool"
	"github.com/goccy/go-yaml"
)

type Config struct {
	DefaultRepository string                       `yaml:"default_repository,omitempty"`
	Mode              string                       `yaml:"mode,omitempty"`
	Managed           ManagedConfig                `yaml:"managed,omitempty"`
	DeviceAgent       DeviceAgentConfig            `yaml:"device_agent,omitempty"`
	Server            ServerConfig                 `yaml:"server"`
	Telegram          TelegramConfig               `yaml:"telegram"`
	Providers         map[string]ProviderConfig    `yaml:"providers"`
	Repositories      map[string]RepositoryProfile `yaml:"repositories"`
	Spool             SpoolConfig                  `yaml:"spool"`
}

type ManagedConfig struct {
	GatewayURL     string `yaml:"gateway_url,omitempty"`
	OrganizationID string `yaml:"organization_id,omitempty"`
	DeviceID       string `yaml:"device_id,omitempty"`
	IdentityPath   string `yaml:"identity_path,omitempty"`
	RecordPath     string `yaml:"record_path,omitempty"`
	StatePath      string `yaml:"state_path,omitempty"`
}

// DeviceAgentConfig enables the same binary to expose the paired-device WSS
// processor on a headless execution host. It is optional; the local Mac
// controller leaves it disabled.
type DeviceAgentConfig struct {
	Enabled                 bool   `yaml:"enabled,omitempty"`
	Listen                  string `yaml:"listen,omitempty"`
	OrganizationID          string `yaml:"organization_id,omitempty"`
	DeviceID                string `yaml:"device_id,omitempty"`
	IdentityPath            string `yaml:"identity_path,omitempty"`
	ControllerPublicKeyPath string `yaml:"controller_public_key_path,omitempty"`
	TLSCertPath             string `yaml:"tls_cert_path,omitempty"`
	TLSKeyPath              string `yaml:"tls_key_path,omitempty"`
	ResultsPath             string `yaml:"results_path,omitempty"`
	ReplayStatePath         string `yaml:"replay_state_path,omitempty"`
	ConnectionEpoch         uint64 `yaml:"connection_epoch,omitempty"`
	ControllerEpoch         uint64 `yaml:"controller_epoch,omitempty"`
}

type SpoolConfig struct {
	MaxBytes               int64 `yaml:"max_bytes"`
	WarningWatermarkBytes  int64 `yaml:"warning_watermark_bytes"`
	CriticalWatermarkBytes int64 `yaml:"critical_watermark_bytes"`
	CriticalReserveBytes   int64 `yaml:"critical_reserve_bytes"`
}

func (c SpoolConfig) Normalize() SpoolConfig {
	defaults := spool.DefaultConfig()
	if c.MaxBytes <= 0 {
		c.MaxBytes = defaults.MaxBytes
	}
	if c.WarningWatermarkBytes <= 0 {
		c.WarningWatermarkBytes = defaults.WarningWatermarkBytes
	}
	if c.CriticalWatermarkBytes <= 0 {
		c.CriticalWatermarkBytes = defaults.CriticalWatermarkBytes
	}
	if c.CriticalReserveBytes <= 0 {
		c.CriticalReserveBytes = defaults.CriticalReserveBytes
	}
	return c
}

func (c SpoolConfig) Domain() spool.Config {
	return spool.Config{MaxBytes: c.MaxBytes, WarningWatermarkBytes: c.WarningWatermarkBytes, CriticalWatermarkBytes: c.CriticalWatermarkBytes, CriticalReserveBytes: c.CriticalReserveBytes}
}

type ServerConfig struct {
	Listen                     string   `yaml:"listen"`
	AllowedTailscaleIdentities []string `yaml:"allowed_tailscale_identities"`
}

type TelegramConfig struct {
	PrivateChatOnly bool    `yaml:"private_chat_only"`
	AllowedUserIDs  []int64 `yaml:"allowed_user_ids"`
	PairedChatID    int64   `yaml:"paired_chat_id"`
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
	Isolation     IsolationProfile      `yaml:"isolation,omitempty"`
}

type IsolationProfile struct {
	Tier          string                 `yaml:"tier,omitempty"`
	WorktreeRoot  string                 `yaml:"worktree_root,omitempty"`
	WritablePaths []string               `yaml:"writable_paths,omitempty"`
	Network       IsolationNetworkPolicy `yaml:"network,omitempty"`
	Limits        IsolationLimits        `yaml:"limits,omitempty"`
	Automation    IsolationAutomation    `yaml:"automation,omitempty"`
}

type IsolationNetworkPolicy struct {
	Mode     string   `yaml:"mode,omitempty"`
	Provider []string `yaml:"provider,omitempty"`
	Package  []string `yaml:"package,omitempty"`
	Test     []string `yaml:"test,omitempty"`
}

type IsolationLimits struct {
	CPUSeconds    uint64 `yaml:"cpu_seconds,omitempty"`
	MemoryBytes   uint64 `yaml:"memory_bytes,omitempty"`
	FileSizeBytes uint64 `yaml:"file_size_bytes,omitempty"`
	OpenFiles     uint64 `yaml:"open_files,omitempty"`
	Processes     uint64 `yaml:"processes,omitempty"`
}

type IsolationAutomation struct {
	Secrets     bool `yaml:"secrets,omitempty"`
	Network     bool `yaml:"network,omitempty"`
	Publication bool `yaml:"publication,omitempty"`
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
	if cfg.Mode == "" {
		cfg.Mode = "standalone"
	}
	cfg.Spool = cfg.Spool.Normalize()
	if err := cfg.Spool.Domain().Validate(); err != nil {
		return Config{}, fmt.Errorf("spool: %w", err)
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
