package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var shellSafeExecutable = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// EnsureStatuslineSettings merges the task-scoped status-line command into
// Claude's dedicated subscription configuration without replacing unrelated
// user preferences or authentication state.
func EnsureStatuslineSettings(configDir, executable string) error {
	if !filepath.IsAbs(configDir) || !filepath.IsAbs(executable) {
		return errors.New("Claude settings paths must be absolute")
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create Claude config directory: %w", err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return fmt.Errorf("secure Claude config directory: %w", err)
	}
	path := filepath.Join(configDir, "settings.json")
	settings := make(map[string]any)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("Claude settings file is unsafe")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read Claude settings: %w", err)
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("decode Claude settings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Claude settings: %w", err)
	}
	settings["statusLine"] = map[string]any{"type": "command", "command": shellCommand(executable) + " claude-statusline"}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Claude settings: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(configDir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("create Claude settings: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish Claude settings: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func shellCommand(executable string) string {
	if shellSafeExecutable.MatchString(executable) {
		return executable
	}
	return "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
}
