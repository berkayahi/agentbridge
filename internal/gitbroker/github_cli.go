package gitbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/berkayahi/agentbridge/internal/security"
)

type CLIExecutor interface {
	Run(context.Context, string, []string, []string) (string, string, error)
}

type GitHubCLI struct {
	Executable  string
	Environment EnvironmentConfig
	Credentials CredentialSource
	Executor    CLIExecutor
	MaxOutput   int
}

func (g GitHubCLI) Host() string { return "github.com" }

func (g GitHubCLI) CreatePullRequest(ctx context.Context, request PullRequestRequest) (PullRequest, error) {
	if request.Operation.Kind != KindCreatePullRequest || !validID(request.Operation.ID) {
		return PullRequest{}, ErrInvalidOperation
	}
	slug, err := repositorySlug(request.Repository.RemoteURL)
	if err != nil {
		return PullRequest{}, err
	}
	base, err := branchName(request.BaseRef)
	if err != nil {
		return PullRequest{}, err
	}
	head, err := branchName(request.HeadRef)
	if err != nil {
		return PullRequest{}, err
	}
	if strings.TrimSpace(request.Title) == "" || strings.ContainsAny(request.Title+request.Body, "\x00") {
		return PullRequest{}, ErrInvalidOperation
	}
	args := []string{"pr", "create", "--repo", slug, "--base", base, "--head", head, "--title", request.Title, "--body", request.Body}
	stdout, _, err := g.run(ctx, request.Operation.ID, request.CredentialRef, args, true)
	if err != nil {
		return PullRequest{}, err
	}
	url := firstGitHubURL(stdout)
	if url == "" {
		return PullRequest{}, errors.New("gitbroker: GitHub PR create returned no URL")
	}
	number, err := parsePRNumber(url)
	if err != nil {
		return PullRequest{}, err
	}
	return PullRequest{Number: number, URL: url, Title: request.Title, HeadRef: head, BaseRef: base, ProviderID: providerID(number)}, nil
}

func (g GitHubCLI) ReadPullRequest(ctx context.Context, request ReadPullRequestRequest) (PullRequest, error) {
	if request.Operation.Kind != KindReadPullRequest || validatePRNumber(request.Number) != nil {
		return PullRequest{}, ErrInvalidOperation
	}
	slug, err := repositorySlug(request.Repository.RemoteURL)
	if err != nil {
		return PullRequest{}, err
	}
	args := []string{"pr", "view", strconv.FormatInt(request.Number, 10), "--repo", slug, "--json", "number,url,title,state,headRefName,baseRefName,mergeCommit"}
	stdout, _, err := g.run(ctx, request.Operation.ID, request.CredentialRef, args, false)
	if err != nil {
		return PullRequest{}, err
	}
	var value struct {
		Number      int64  `json:"number"`
		URL         string `json:"url"`
		Title       string `json:"title"`
		State       string `json:"state"`
		HeadRef     string `json:"headRefName"`
		BaseRef     string `json:"baseRefName"`
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal([]byte(stdout), &value); err != nil {
		return PullRequest{}, fmt.Errorf("gitbroker: decode GitHub PR: %w", err)
	}
	return PullRequest{Number: value.Number, URL: value.URL, Title: value.Title, State: value.State, HeadRef: value.HeadRef, BaseRef: value.BaseRef, MergeCommit: value.MergeCommit.OID, ProviderID: providerID(value.Number)}, nil
}

func (g GitHubCLI) SubmitReview(ctx context.Context, request ReviewRequest) error {
	if request.Operation.Kind != KindSubmitReview || validatePRNumber(request.Number) != nil {
		return ErrInvalidOperation
	}
	slug, err := repositorySlug(request.Repository.RemoteURL)
	if err != nil {
		return err
	}
	flag := map[string]string{"approve": "--approve", "comment": "--comment", "request_changes": "--request-changes"}[request.Decision]
	if flag == "" {
		return ErrInvalidOperation
	}
	args := []string{"pr", "review", strconv.FormatInt(request.Number, 10), "--repo", slug, flag, "--body", request.Body}
	_, _, err = g.run(ctx, request.Operation.ID, request.CredentialRef, args, true)
	return err
}

func (g GitHubCLI) Merge(ctx context.Context, request MergeRequest) error {
	if request.Operation.Kind != KindMerge || validatePRNumber(request.Number) != nil {
		return ErrInvalidOperation
	}
	slug, err := repositorySlug(request.Repository.RemoteURL)
	if err != nil {
		return err
	}
	flag := map[string]string{"merge": "--merge", "squash": "--squash", "rebase": "--rebase"}[request.Method]
	if flag == "" {
		return ErrInvalidOperation
	}
	args := []string{"pr", "merge", strconv.FormatInt(request.Number, 10), "--repo", slug, flag, "--delete-branch=false"}
	_, _, err = g.run(ctx, request.Operation.ID, request.CredentialRef, args, true)
	return err
}

func (g GitHubCLI) run(ctx context.Context, operationID, credentialRef string, args []string, reconcile bool) (string, string, error) {
	for _, arg := range args {
		if strings.ContainsRune(arg, '\x00') || strings.HasPrefix(arg, "-") && strings.ContainsAny(arg, ";`$()") {
			return "", "", ErrInvalidOperation
		}
	}
	credential, err := g.credential(ctx, credentialRef)
	if err != nil {
		return "", "", err
	}
	env, cleanup, err := g.Environment.WithCommandCredential(credential)
	if err != nil {
		return "", "", err
	}
	defer cleanup()
	executor := g.Executor
	if executor == nil {
		executor = systemCLIExecutor{}
	}
	executable := g.Executable
	if executable == "" {
		executable = "gh"
	}
	stdout, stderr, err := executor.Run(ctx, executable, args, env)
	redactor := security.NewRedactor(security.Config{Secrets: []string{credential.Value()}})
	stdout, stderr = redactor.RedactString(stdout), redactor.RedactString(stderr)
	if err == nil {
		return stdout, stderr, nil
	}
	if reconcile && (errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) || transportFailure(stderr)) {
		return stdout, stderr, ReconciliationRequiredError{OperationID: operationID, Cause: err}
	}
	return stdout, stderr, fmt.Errorf("gitbroker: GitHub CLI request failed: %w: %s", err, stderr)
}

func (g GitHubCLI) credential(ctx context.Context, reference string) (Credential, error) {
	if reference == "" {
		return Credential{}, nil
	}
	if g.Credentials == nil {
		return Credential{}, ErrCredentialUnavailable
	}
	return g.Credentials.Get(ctx, reference)
}

type systemCLIExecutor struct{}

func (systemCLIExecutor) Run(ctx context.Context, executable string, args []string, env []string) (string, string, error) {
	if filepath.IsAbs(executable) == false && executable != "gh" {
		return "", "", ErrInvalidOperation
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = env
	var stdout, stderr limitedOutput
	stdout.limit, stderr.limit = 64<<10, 64<<10
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

type limitedOutput struct {
	value     strings.Builder
	limit     int
	truncated bool
}

func (o *limitedOutput) Write(data []byte) (int, error) {
	if o.value.Len() < o.limit {
		remaining := o.limit - o.value.Len()
		if len(data) > remaining {
			_, _ = o.value.Write(data[:remaining])
			o.truncated = true
		} else {
			_, _ = o.value.Write(data)
		}
	} else if len(data) > 0 {
		o.truncated = true
	}
	return len(data), nil
}
func (o *limitedOutput) String() string {
	if o.truncated {
		return o.value.String() + "…[TRUNCATED]"
	}
	return o.value.String()
}

func firstGitHubURL(output string) string {
	for _, value := range strings.Fields(output) {
		if strings.HasPrefix(value, "https://github.com/") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parsePRNumber(raw string) (int64, error) {
	parts := strings.Split(strings.TrimSuffix(raw, "/"), "/")
	if len(parts) == 0 {
		return 0, ErrInvalidOperation
	}
	value, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || value <= 0 {
		return 0, ErrInvalidOperation
	}
	return value, nil
}

func transportFailure(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, marker := range []string{"timeout", "timed out", "connection reset", "connection refused", "network is unreachable", "temporary failure", "tls"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

var _ HostingAdapter = GitHubCLI{}
