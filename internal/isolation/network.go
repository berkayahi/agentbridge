package isolation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type NetworkMode string

const (
	NetworkDeny      NetworkMode = "deny"
	NetworkAllowlist NetworkMode = "allowlist"
)

type NetworkPolicy struct {
	Mode     NetworkMode `json:"mode" yaml:"mode"`
	Provider []string    `json:"provider,omitempty" yaml:"provider,omitempty"`
	Package  []string    `json:"package,omitempty" yaml:"package,omitempty"`
	Test     []string    `json:"test,omitempty" yaml:"test,omitempty"`
}

type DestinationKind string

const (
	DestinationProvider DestinationKind = "provider"
	DestinationPackage  DestinationKind = "package"
	DestinationTest     DestinationKind = "test"
)

var (
	ErrNetworkDenied         = errors.New("isolation: outbound network denied")
	ErrUnapprovedDestination = errors.New("isolation: destination is not approved")
	ErrUnsafeDestination     = errors.New("isolation: destination resolves to a prohibited address")
)

func (p NetworkPolicy) Validate() error {
	if p.Mode != "" && p.Mode != NetworkDeny && p.Mode != NetworkAllowlist {
		return fmt.Errorf("unknown network mode %q", p.Mode)
	}
	if p.Mode == NetworkAllowlist && len(p.Provider)+len(p.Package)+len(p.Test) == 0 {
		return errors.New("allowlist must contain at least one destination")
	}
	for _, host := range append(append(append([]string(nil), p.Provider...), p.Package...), p.Test...) {
		if err := validateHostname(host); err != nil {
			return err
		}
	}
	return nil
}

func (p NetworkPolicy) ApprovedHosts() []string {
	values := append(append(append([]string(nil), p.Provider...), p.Package...), p.Test...)
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, host := range values {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if _, ok := seen[host]; ok || host == "" {
			continue
		}
		seen[host] = struct{}{}
		result = append(result, host)
	}
	sort.Strings(result)
	return result
}

func ValidateURL(raw string, policy NetworkPolicy) error {
	if policy.Mode == "" || policy.Mode == NetworkDeny {
		return ErrNetworkDenied
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return ErrUnapprovedDestination
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if net.ParseIP(host) != nil || !approvedHost(host, policy.ApprovedHosts()) {
		return ErrUnapprovedDestination
	}
	return nil
}

func ResolveApproved(ctx context.Context, policy NetworkPolicy, resolver *net.Resolver) (map[string][]net.IP, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if policy.Mode == NetworkDeny || policy.Mode == "" {
		return map[string][]net.IP{}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	result := make(map[string][]net.IP)
	for _, host := range policy.ApprovedHosts() {
		addresses, err := resolver.LookupIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return nil, fmt.Errorf("resolve approved destination %q: %w", host, err)
		}
		for _, address := range addresses {
			if prohibitedIP(address) {
				return nil, fmt.Errorf("%w: %s", ErrUnsafeDestination, host)
			}
		}
		result[host] = append([]net.IP(nil), addresses...)
	}
	return result, nil
}

func NewHTTPClient(ctx context.Context, policy NetworkPolicy, resolver *net.Resolver) (*http.Client, error) {
	resolved, err := ResolveApproved(ctx, policy, resolver)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrUnapprovedDestination
		}
		if policy.Mode != NetworkAllowlist {
			return nil, ErrNetworkDenied
		}
		allowed := resolved[strings.ToLower(strings.TrimSuffix(host, "."))]
		if len(allowed) == 0 {
			return nil, ErrUnapprovedDestination
		}
		for _, candidate := range allowed {
			if conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.String(), port)); err == nil {
				return conn, nil
			}
		}
		return nil, ErrUnsafeDestination
	}}
	return &http.Client{Transport: transport, CheckRedirect: func(next *http.Request, _ []*http.Request) error {
		return ValidateURL(next.URL.String(), policy)
	}}, nil
}

func approvedHost(host string, approved []string) bool {
	for _, candidate := range approved {
		if host == candidate {
			return true
		}
	}
	return false
}

func validateHostname(host string) error {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(host, "/\\:@") || !strings.Contains(host, ".") {
		return ErrUnapprovedDestination
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return ErrUnapprovedDestination
		}
		for _, value := range label {
			if !(value == '-' || value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9') {
				return ErrUnapprovedDestination
			}
		}
	}
	return nil
}

func prohibitedIP(address net.IP) bool {
	if address == nil || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() || address.IsMulticast() {
		return true
	}
	blocked := []net.IPNet{
		{IP: net.ParseIP("169.254.169.254"), Mask: net.CIDRMask(32, 32)},
		{IP: net.ParseIP("169.254.170.2"), Mask: net.CIDRMask(32, 32)},
		{IP: net.ParseIP("100.100.100.200"), Mask: net.CIDRMask(32, 32)},
	}
	for _, network := range blocked {
		if network.Contains(address) {
			return true
		}
	}
	return false
}
