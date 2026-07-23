package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

func (c Config) Validate() error {
	if c.Mode != "standalone" && c.Mode != "managed" {
		return errors.New("mode must be standalone or managed")
	}
	if err := c.Managed.validate(c.Mode); err != nil {
		return err
	}
	if err := validateListen(c.Server.Listen); err != nil {
		return fmt.Errorf("server listen: %w", err)
	}
	if len(c.Server.AllowedTailscaleIdentities) == 0 {
		return errors.New("server allowed_tailscale_identities must not be empty")
	}
	identities := make(map[string]struct{}, len(c.Server.AllowedTailscaleIdentities))
	for _, identity := range c.Server.AllowedTailscaleIdentities {
		if identity == "" || identity != strings.TrimSpace(identity) {
			return errors.New("server allowed_tailscale_identities must contain trimmed, nonempty values")
		}
		if _, exists := identities[identity]; exists {
			return errors.New("server allowed_tailscale_identities must not contain duplicates")
		}
		identities[identity] = struct{}{}
	}
	if err := c.Telegram.validate(); err != nil {
		return err
	}
	if len(c.Providers) == 0 {
		return errors.New("providers must not be empty")
	}
	for name, provider := range c.Providers {
		if !namePattern.MatchString(name) {
			return errors.New("provider name is invalid")
		}
		if name != "codex" && name != "claude" {
			return errors.New("provider runtime must be explicitly codex or claude")
		}
		if !filepath.IsAbs(provider.Executable) {
			return errors.New("provider executable must be absolute")
		}
		if !modelPattern.MatchString(provider.Model) {
			return errors.New("provider model must be a safe nonempty model identifier")
		}
		if name == "codex" && !supportedCodexModel(provider.Model) {
			return errors.New("Codex model must be GPT-5.6 Terra or a higher Terra/Sol model")
		}
	}
	if len(c.Repositories) == 0 {
		return errors.New("repositories must contain at least one profile")
	}
	for name, profile := range c.Repositories {
		if !namePattern.MatchString(name) {
			return errors.New("repository profile name is invalid")
		}
		if err := profile.validate(); err != nil {
			return fmt.Errorf("repository profile %q: %w", name, err)
		}
	}
	if !namePattern.MatchString(c.DefaultRepository) {
		return errors.New("default_repository must name a configured repository")
	}
	if _, ok := c.Repositories[c.DefaultRepository]; !ok {
		return errors.New("default_repository must name a configured repository")
	}
	return nil
}

func (m ManagedConfig) validate(mode string) error {
	if mode != "managed" {
		return nil
	}
	parsed, err := url.ParseRequestURI(strings.TrimSpace(m.GatewayURL))
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil {
		return errors.New("managed gateway_url must be a WSS URL without user info")
	}
	for name, value := range map[string]string{"organization_id": m.OrganizationID, "device_id": m.DeviceID} {
		if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") || len(value) > 128 {
			return fmt.Errorf("managed %s must be a trimmed nonempty identifier", name)
		}
	}
	for name, path := range map[string]string{"identity_path": m.IdentityPath, "record_path": m.RecordPath, "state_path": m.StatePath} {
		if path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("managed %s must be absolute", name)
		}
	}
	return nil
}

func supportedCodexModel(model string) bool {
	parts := codexModelPattern.FindStringSubmatch(model)
	if len(parts) != 4 {
		return false
	}
	major, _ := strconv.Atoi(parts[1])
	minor, _ := strconv.Atoi(parts[2])
	return major > 5 || major == 5 && minor >= 6
}

func validateListen(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("must be a loopback address with a valid port")
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("must bind to loopback only")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("must be a loopback address with a valid port")
	}
	return nil
}

func (t TelegramConfig) validate() error {
	if !t.PrivateChatOnly {
		return errors.New("telegram private_chat_only must be true")
	}
	if len(t.AllowedUserIDs) != 1 {
		return errors.New("telegram allowed_user_ids must contain exactly one paired operator")
	}
	seen := make(map[int64]struct{}, len(t.AllowedUserIDs))
	for _, id := range t.AllowedUserIDs {
		if id <= 0 {
			return errors.New("telegram allowed_user_ids must contain only positive IDs")
		}
		if _, exists := seen[id]; exists {
			return errors.New("telegram allowed_user_ids must not contain duplicates")
		}
		seen[id] = struct{}{}
	}
	if t.PairedChatID <= 0 {
		return errors.New("telegram paired_chat_id must be positive")
	}
	return nil
}

func (r RepositoryProfile) validate() error {
	if !filepath.IsAbs(r.CheckoutPath) {
		return errors.New("checkout_path must be absolute")
	}
	if strings.TrimSpace(r.Remote) == "" {
		return errors.New("remote must not be empty")
	}
	if !isHeadRef(r.BaseRef) {
		return errors.New("base_ref must be an exact refs/heads/<branch> ref")
	}
	if len(r.Verification) == 0 {
		return errors.New("verification must contain at least one command")
	}
	for _, command := range r.Verification {
		if err := command.validate(); err != nil {
			return err
		}
	}
	if err := validateDeploymentURL(r.DeploymentURL); err != nil {
		return err
	}
	if err := r.Isolation.validate(); err != nil {
		return err
	}
	return r.Delivery.validate()
}

func (i IsolationProfile) validate() error {
	if i.Tier != "" && i.Tier != "strong" && i.Tier != "standard" && i.Tier != "weak" {
		return errors.New("isolation tier must be strong, standard, or weak")
	}
	if i.WorktreeRoot != "" && !filepath.IsAbs(i.WorktreeRoot) {
		return errors.New("isolation worktree_root must be absolute")
	}
	for _, path := range i.WritablePaths {
		if path == "" || !filepath.IsAbs(path) {
			return errors.New("isolation writable_paths must contain absolute paths")
		}
	}
	if i.Network.Mode != "" && i.Network.Mode != "deny" && i.Network.Mode != "allowlist" {
		return errors.New("isolation network mode must be deny or allowlist")
	}
	if i.Network.Mode == "allowlist" && len(i.Network.Provider)+len(i.Network.Package)+len(i.Network.Test) == 0 {
		return errors.New("isolation network allowlist must not be empty")
	}
	for _, host := range append(append(append([]string(nil), i.Network.Provider...), i.Network.Package...), i.Network.Test...) {
		if err := validateIsolationHost(host); err != nil {
			return err
		}
	}
	if i.Tier == "weak" && (i.Automation.Secrets || i.Automation.Network || i.Automation.Publication) {
		return errors.New("weak isolation cannot enable automatic secrets, network, or publication")
	}
	return nil
}

func validateIsolationHost(host string) error {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" || strings.ContainsAny(host, "/\\:@") || strings.Contains(host, "..") {
		return errors.New("isolation network destination is invalid")
	}
	for _, value := range host {
		if !(value == '-' || value == '_' || value == '.' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9') {
			return errors.New("isolation network destination is invalid")
		}
	}
	return nil
}

func (v VerificationCommand) validate() error {
	if len(v.Argv) == 0 {
		return errors.New("verification argv must not be empty")
	}
	for _, arg := range v.Argv {
		if arg == "" {
			return errors.New("verification argv must not contain empty arguments")
		}
	}
	if v.Dir == "" || v.Dir == "." {
		return nil
	}
	if filepath.IsAbs(v.Dir) {
		return errors.New("verification dir must be relative")
	}
	clean := filepath.Clean(v.Dir)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("verification dir cannot traverse outside the checkout")
	}
	return nil
}

func validateDeploymentURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("deployment_url must be a valid HTTP or HTTPS URL")
	}
	return nil
}

func (d DeliveryPolicy) validate() error {
	if !d.Enabled {
		if d.AllowedRef != "" {
			return errors.New("delivery allowed_ref must be empty when delivery is disabled")
		}
		return nil
	}
	if !isHeadRef(d.AllowedRef) || isProductionRef(d.AllowedRef) {
		return errors.New("delivery allowed_ref must be a safe exact refs/heads/<branch> ref")
	}
	return nil
}

func isHeadRef(ref string) bool {
	const prefix = "refs/heads/"
	branch := strings.TrimPrefix(ref, prefix)
	if branch == ref || branch == "" || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") {
		return false
	}
	for i := 0; i < len(branch); i++ {
		if branch[i] < 0x20 || branch[i] == 0x7f {
			return false
		}
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") || strings.ContainsAny(branch, " ~^:?*[\\") {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func isProductionRef(ref string) bool {
	switch strings.ToLower(ref) {
	case "refs/heads/main", "refs/heads/master", "refs/heads/production":
		return true
	default:
		return false
	}
}
