package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
)

// ErrRepositoryExists means the configuration already names this repository.
// Registering over it would silently change where an id points, and every task
// ever dispatched against that id resolves through it.
var ErrRepositoryExists = errors.New("config: repository is already configured")

// AddRepository adds one repository to an existing configuration file.
//
// Configuration is the thing that grants a bee access to a keeper's code, so it
// is written here rather than by the product surface: this package already owns
// the schema and the validation, and Load refuses unknown fields, which means a
// round trip through Config cannot silently drop something it does not know
// about. A writer living anywhere else would have to reimplement that promise.
//
// The whole configuration is validated after the insertion, not just the new
// entry, because a repository can be individually valid and still make the file
// invalid — a delivery ref that collides with another profile's, for instance.
func AddRepository(path, id string, profile RepositoryProfile) error {
	if !filepath.IsAbs(path) {
		return errors.New("config: configuration path must be absolute")
	}
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	if _, exists := cfg.Repositories[id]; exists {
		return fmt.Errorf("%w: %s", ErrRepositoryExists, id)
	}
	if cfg.Repositories == nil {
		cfg.Repositories = map[string]RepositoryProfile{}
	}
	cfg.Repositories[id] = profile
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: adding %s would make the configuration invalid: %w", id, err)
	}

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: encode configuration: %w", err)
	}
	return replaceOwnerOnly(path, body)
}

// replaceOwnerOnly writes through a temporary file in the same directory and
// renames it into place, so an interrupted write cannot leave a keeper with half
// a configuration — and therefore with a hive that will not start.
func replaceOwnerOnly(path string, body []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, body, 0o600); err != nil {
		return fmt.Errorf("config: write configuration: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("config: replace configuration: %w", err)
	}
	return nil
}
