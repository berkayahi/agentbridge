package gitbroker

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var ErrUnsupportedHost = errors.New("unsupported_host")

type UnsupportedHostError struct{ Host string }

func (e UnsupportedHostError) Error() string {
	return fmt.Sprintf("%s: %s", ErrUnsupportedHost, e.Host)
}
func (e UnsupportedHostError) Unwrap() error { return ErrUnsupportedHost }

type ReconciliationRequiredError struct {
	OperationID string
	Cause       error
}

func (e ReconciliationRequiredError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: operation %s", ErrReconciliationRequired, e.OperationID)
	}
	return fmt.Sprintf("%s: operation %s: %v", ErrReconciliationRequired, e.OperationID, e.Cause)
}
func (e ReconciliationRequiredError) Unwrap() error { return ErrReconciliationRequired }

type PullRequest struct {
	Number      int64
	URL         string
	Title       string
	State       string
	HeadRef     string
	BaseRef     string
	MergeCommit string
	ProviderID  string
}

type PullRequestRequest struct {
	Operation     Operation
	Repository    RepositoryConfig
	BaseRef       string
	HeadRef       string
	Title         string
	Body          string
	CredentialRef string
}

type ReadPullRequestRequest struct {
	Operation     Operation
	Repository    RepositoryConfig
	Number        int64
	CredentialRef string
}

type ReviewRequest struct {
	Operation     Operation
	Repository    RepositoryConfig
	Number        int64
	Decision      string
	Body          string
	CredentialRef string
}

type MergeRequest struct {
	Operation     Operation
	Repository    RepositoryConfig
	Number        int64
	Method        string
	CredentialRef string
}

type HostingAdapter interface {
	Host() string
	CreatePullRequest(context.Context, PullRequestRequest) (PullRequest, error)
	ReadPullRequest(context.Context, ReadPullRequestRequest) (PullRequest, error)
	SubmitReview(context.Context, ReviewRequest) error
	Merge(context.Context, MergeRequest) error
}

type HostingRegistry struct{ adapters map[string]HostingAdapter }

func NewHostingRegistry(adapters ...HostingAdapter) (*HostingRegistry, error) {
	result := &HostingRegistry{adapters: make(map[string]HostingAdapter, len(adapters))}
	for _, adapter := range adapters {
		if adapter == nil || strings.TrimSpace(adapter.Host()) == "" {
			return nil, ErrUnsupportedHost
		}
		host := strings.ToLower(adapter.Host())
		if _, exists := result.adapters[host]; exists {
			return nil, fmt.Errorf("duplicate hosting adapter %q", host)
		}
		result.adapters[host] = adapter
	}
	return result, nil
}

func (r *HostingRegistry) Resolve(remoteURL string) (HostingAdapter, error) {
	host, err := remoteHost(remoteURL)
	if err != nil {
		return nil, err
	}
	adapter, ok := r.adapters[host]
	if !ok {
		return nil, UnsupportedHostError{Host: host}
	}
	return adapter, nil
}

func remoteHost(raw string) (string, error) {
	if strings.HasPrefix(raw, "git@") {
		parts := strings.SplitN(raw, ":", 2)
		if len(parts) != 2 {
			return "", ErrUnsupportedHost
		}
		at := strings.IndexByte(parts[0], '@')
		if at < 1 || at == len(parts[0])-1 {
			return "", ErrUnsupportedHost
		}
		return strings.ToLower(parts[0][at+1:]), nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", ErrUnsupportedHost
	}
	return strings.ToLower(parsed.Hostname()), nil
}

func repositorySlug(raw string) (string, error) {
	host, err := remoteHost(raw)
	if err != nil || host != "github.com" {
		return "", UnsupportedHostError{Host: host}
	}
	path := raw
	if strings.HasPrefix(raw, "git@") {
		parts := strings.SplitN(raw, ":", 2)
		path = parts[1]
	} else if parsed, parseErr := url.Parse(raw); parseErr == nil {
		path = parsed.Path
	}
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !validSlug(parts[0]) || !validSlug(parts[1]) {
		return "", ErrUnsupportedHost
	}
	return parts[0] + "/" + parts[1], nil
}

func validSlug(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("._-", r) {
			return false
		}
	}
	return true
}

func branchName(ref string) (string, error) {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(ref, prefix) || len(ref) == len(prefix) || !strings.HasPrefix(ref, "refs/") {
		return "", ErrInvalidOperation
	}
	return strings.TrimPrefix(ref, prefix), nil
}

func validatePRNumber(number int64) error {
	if number <= 0 {
		return ErrInvalidOperation
	}
	return nil
}

func providerID(number int64) string { return strconv.FormatInt(number, 10) }

var _ = errors.Is
