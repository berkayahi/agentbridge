package isolation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type EnvironmentPolicy struct {
	Allowed map[string]struct{}
	Extra   map[string]string
}

// FilterEnvironment constructs an explicit environment and never falls back
// to inheritance. Provider credentials, credential helpers, sockets, cloud
// tokens, and inherited AgentBridge scopes are removed even when an allowlist
// accidentally includes them.
func FilterEnvironment(base []string, policy EnvironmentPolicy) []string {
	values := make(map[string]string, len(base)+len(policy.Extra))
	for _, entry := range base {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !safeEnvironmentName(name) || isCredentialEnvironment(name) {
			continue
		}
		if policy.Allowed != nil {
			if _, allowed := policy.Allowed[name]; !allowed {
				continue
			}
		} else if !defaultEnvironmentName(name) {
			continue
		}
		values[name] = value
	}
	for name, value := range policy.Extra {
		// Extra values are explicitly supplied by the trusted process boundary;
		// this is different from allowing an inherited AGENTBRIDGE_* scope or
		// provider credential through the base environment.
		if safeEnvironmentName(name) {
			values[name] = value
		}
	}
	keys := make([]string, 0, len(values))
	for name := range values {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, name := range keys {
		result = append(result, name+"="+values[name])
	}
	return result
}

func isCredentialEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	if strings.HasPrefix(upper, "AGENTBRIDGE_") || strings.HasPrefix(upper, "AWS_") || strings.HasPrefix(upper, "AZURE_") || strings.HasPrefix(upper, "GOOGLE_") || strings.HasPrefix(upper, "GCP_") || strings.HasPrefix(upper, "DOCKER_") {
		return true
	}
	switch upper {
	case "SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_ASKPASS", "GIT_TERMINAL_PROMPT", "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_PROXY_COMMAND", "KUBECONFIG", "NPM_TOKEN", "NODE_AUTH_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "HF_TOKEN", "HUGGINGFACE_TOKEN":
		return true
	}
	for _, suffix := range []string{"_TOKEN", "_API_KEY", "_SECRET", "_PASSWORD", "_PASSPHRASE", "_CREDENTIAL", "_CREDENTIALS", "_AUTH_TOKEN"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

func safeEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, value := range name {
		if (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z') || (index > 0 && value >= '0' && value <= '9') || value == '_' {
			continue
		}
		return false
	}
	return true
}

func defaultEnvironmentName(name string) bool {
	if isCredentialEnvironment(name) {
		return false
	}
	switch name {
	case "HOME", "PATH", "TMPDIR", "TMP", "TEMP", "USER", "LOGNAME", "SHELL", "TERM", "PWD", "LANG", "LC_ALL", "SYSTEMROOT", "WINDIR", "GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "CI", "GO_WANT_CLAUDE_HELPER":
		return true
	}
	return strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "GO_")
}

func PrivateDirectory(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("isolation: private directory must be absolute")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("isolation: private directory cannot be a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect private directory: %w", err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("isolation: private directory is unsafe")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure private directory: %w", err)
	}
	return nil
}

func CreatePrivateFile(dir, prefix string, contents []byte) (*os.File, error) {
	if err := PrivateDirectory(dir); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return nil, fmt.Errorf("create private file: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if err := file.Chmod(0o600); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := file.Write(contents); err != nil {
		cleanup()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		cleanup()
		return nil, err
	}
	return file, nil
}
