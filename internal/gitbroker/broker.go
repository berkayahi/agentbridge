package gitbroker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bridgegit "github.com/berkayahi/agentbridge/internal/git"
	"github.com/berkayahi/agentbridge/internal/secretscan"
)

type GitExecutor interface {
	Run(context.Context, string, ...string) (bridgegit.RunResult, error)
}

var ErrMissingDependency = errors.New("gitbroker: required dependency is missing")

type Config struct {
	Registry     *Registry
	Repositories []RepositoryConfig
	Git          GitExecutor
	Journal      Journal
	Signer       ReceiptSigner
	Scanner      secretscan.Scanner
	Credentials  CredentialSource
	Environment  EnvironmentConfig
	Hosting      *HostingRegistry
	Checkpoints  CheckpointStore
	Clock        func() time.Time
}

type Broker struct {
	registry    *Registry
	git         GitExecutor
	journal     Journal
	signer      ReceiptSigner
	scanner     secretscan.Scanner
	credentials CredentialSource
	environment EnvironmentConfig
	hosting     *HostingRegistry
	checkpoints CheckpointStore
	clock       func() time.Time
}

func New(cfg Config) (*Broker, error) {
	registry := cfg.Registry
	if registry == nil {
		var err error
		registry, err = NewRegistry(cfg.Repositories...)
		if err != nil {
			return nil, err
		}
	}
	if cfg.Journal == nil || cfg.Signer == nil {
		return nil, ErrMissingDependency
	}
	environment, err := cfg.Environment.Base()
	if err != nil {
		return nil, err
	}
	gitExecutor := cfg.Git
	if gitExecutor == nil {
		gitExecutor = bridgegit.Runner{Environment: environment}
	}
	scanner := cfg.Scanner
	if scanner == nil {
		scannerValue := secretscan.NewDetector(secretscan.Config{})
		scanner = scannerValue
	}
	hosting := cfg.Hosting
	if hosting == nil {
		hosting, err = NewHostingRegistry(GitHubCLI{Environment: cfg.Environment, Credentials: cfg.Credentials})
		if err != nil {
			return nil, err
		}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Broker{registry: registry, git: gitExecutor, journal: cfg.Journal, signer: cfg.Signer, scanner: scanner, credentials: cfg.Credentials, environment: cfg.Environment, hosting: hosting, checkpoints: cfg.Checkpoints, clock: clock}, nil
}

func (b *Broker) Checkpoint(ctx context.Context, request CheckpointRequest) (Checkpoint, Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Validate() != nil {
		return Checkpoint{}, Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil {
		return Checkpoint{}, Receipt{}, err
	}
	if done {
		return Checkpoint{ID: request.Operation.ID + "-checkpoint", OperationID: request.Operation.ID, RepositoryID: request.Operation.RepositoryID, CommitSHA: receipt.AfterSHA, BaseSHA: receipt.BeforeSHA, CreatedAt: receipt.FinishedAt}, receipt, nil
	}
	repo, worktree, executor, cleanup, err := b.prepare(ctx, request.Operation, "")
	if err != nil {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	defer cleanup()
	base, err := b.head(ctx, executor, worktree)
	if err != nil || request.Operation.ExpectedBaseSHA != base {
		if err == nil {
			err = ErrDiverged
		}
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if _, err := executor.Run(ctx, worktree, "add", "-A"); err != nil {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if err := b.scan(ctx, worktree, request.ArtifactPaths); err != nil {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	treeResult, err := executor.Run(ctx, worktree, "write-tree")
	if err != nil {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	tree := strings.TrimSpace(treeResult.Stdout)
	if !validGitObjectID(tree) || request.ExpectedTreeSHA != "" && request.ExpectedTreeSHA != tree {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, ErrDiverged)
	}
	commitResult, err := executor.Run(ctx, worktree, "commit-tree", tree, "-p", base, "-m", request.Message)
	if err != nil {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	commit := strings.TrimSpace(commitResult.Stdout)
	if !validGitObjectID(commit) {
		return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, ErrDiverged)
	}
	checkpoint := Checkpoint{ID: request.Operation.ID + "-checkpoint", OperationID: request.Operation.ID, RepositoryID: repo.ID, WorktreeID: request.Operation.WorktreeID, BaseSHA: base, TreeSHA: tree, CommitSHA: commit, CreatedAt: b.clock().UTC()}
	if b.checkpoints != nil {
		if err := b.checkpoints.SaveCheckpoint(ctx, checkpoint); err != nil {
			return Checkpoint{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
		}
	}
	receipt = b.successReceipt(request.Operation, receipt, base, commit, "", commitResult)
	if err := b.saveReceipt(receipt); err != nil {
		return Checkpoint{}, Receipt{}, err
	}
	return checkpoint, receipt, nil
}

func (b *Broker) Commit(ctx context.Context, request CommitRequest) (Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Validate() != nil {
		return Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil {
		return Receipt{}, err
	}
	if done {
		return receipt, nil
	}
	_, worktree, executor, cleanup, err := b.prepare(ctx, request.Operation, request.CredentialRef)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	defer cleanup()
	before, err := b.head(ctx, executor, worktree)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	status, err := executor.Run(ctx, worktree, "status", "--porcelain=v1", "-z")
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if status.Stdout == "" {
		return b.finishNoop(ctx, request.Operation, receipt, before)
	}
	if _, err := executor.Run(ctx, worktree, "add", "-A"); err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if err := b.scan(ctx, worktree, request.ArtifactPaths); err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if _, err := executor.Run(ctx, worktree, "commit", "-m", request.Message); err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	after, err := b.head(ctx, executor, worktree)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, before, after, "", status)
	return receipt, b.saveReceipt(receipt)
}

func (b *Broker) Fetch(ctx context.Context, request FetchRequest) (Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Validate() != nil {
		return Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil || done {
		return receipt, err
	}
	_, worktree, executor, cleanup, err := b.prepare(ctx, request.Operation, request.CredentialRef)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	defer cleanup()
	result, err := executor.Run(ctx, worktree, "fetch", "--no-tags", "--no-prune", request.Operation.TargetRef)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, "", "", "", result)
	return receipt, b.saveReceipt(receipt)
}

func (b *Broker) Push(ctx context.Context, request PushRequest) (Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Validate() != nil {
		return Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil || done {
		return receipt, err
	}
	repo, worktree, executor, cleanup, err := b.prepare(ctx, request.Operation, request.CredentialRef)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	defer cleanup()
	if err := b.scan(ctx, worktree, request.ArtifactPaths); err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	current, err := b.remoteSHA(ctx, executor, worktree, repo.RemoteName, request.Operation.TargetRef)
	if err != nil || current != request.Operation.ExpectedOldSHA {
		if err == nil {
			err = ErrDiverged
		}
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if request.Operation.ExpectedBaseSHA != "" {
		if _, err := executor.Run(ctx, worktree, "merge-base", "--is-ancestor", request.Operation.ExpectedBaseSHA, request.CommitSHA); err != nil {
			return Receipt{}, b.failed(ctx, request.Operation, receipt, ErrDiverged)
		}
	}
	result, err := executor.Run(ctx, worktree, "push", repo.RemoteName, "HEAD:"+request.Operation.TargetRef)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, fmt.Errorf("%w: %v", ErrDiverged, err))
	}
	after, err := b.remoteSHA(ctx, executor, worktree, repo.RemoteName, request.Operation.TargetRef)
	if err != nil || after != request.CommitSHA {
		if err == nil {
			err = ErrDiverged
		}
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, current, after, "", result)
	return receipt, b.saveReceipt(receipt)
}

func (b *Broker) CreatePullRequest(ctx context.Context, request PullRequestRequest) (PullRequest, Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Operation.Kind != KindCreatePullRequest {
		return PullRequest{}, Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil {
		return PullRequest{}, Receipt{}, err
	}
	if done {
		return PullRequest{ProviderID: receipt.ProviderResponseID}, receipt, nil
	}
	_, worktree, _, cleanup, err := b.prepare(ctx, request.Operation, "")
	if err != nil {
		return PullRequest{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	defer cleanup()
	if err := b.scan(ctx, worktree, nil); err != nil {
		return PullRequest{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	adapter, err := b.hosting.Resolve(request.Repository.RemoteURL)
	if err != nil {
		return PullRequest{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	value, err := adapter.CreatePullRequest(ctx, request)
	if err != nil {
		return PullRequest{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, "", "", value.ProviderID, bridgegit.RunResult{})
	return value, receipt, b.saveReceipt(receipt)
}

func (b *Broker) ReadPullRequest(ctx context.Context, request ReadPullRequestRequest) (PullRequest, Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Operation.Kind != KindReadPullRequest {
		return PullRequest{}, Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil {
		return PullRequest{}, Receipt{}, err
	}
	if done {
		return PullRequest{ProviderID: receipt.ProviderResponseID}, receipt, nil
	}
	adapter, err := b.hosting.Resolve(request.Repository.RemoteURL)
	if err != nil {
		return PullRequest{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	value, err := adapter.ReadPullRequest(ctx, request)
	if err != nil {
		return PullRequest{}, Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, "", "", value.ProviderID, bridgegit.RunResult{})
	return value, receipt, b.saveReceipt(receipt)
}

func (b *Broker) SubmitReview(ctx context.Context, request ReviewRequest) (Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Operation.Kind != KindSubmitReview {
		return Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil || done {
		return receipt, err
	}
	adapter, err := b.hosting.Resolve(request.Repository.RemoteURL)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if err := adapter.SubmitReview(ctx, request); err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, "", "", providerID(request.Number), bridgegit.RunResult{})
	return receipt, b.saveReceipt(receipt)
}

func (b *Broker) Merge(ctx context.Context, request MergeRequest) (Receipt, error) {
	if err := request.Operation.Validate(); err != nil || request.Operation.Kind != KindMerge {
		return Receipt{}, ErrInvalidOperation
	}
	receipt, done, err := b.begin(ctx, request.Operation)
	if err != nil || done {
		return receipt, err
	}
	adapter, err := b.hosting.Resolve(request.Repository.RemoteURL)
	if err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	if err := adapter.Merge(ctx, request); err != nil {
		return Receipt{}, b.failed(ctx, request.Operation, receipt, err)
	}
	receipt = b.successReceipt(request.Operation, receipt, "", "", providerID(request.Number), bridgegit.RunResult{})
	return receipt, b.saveReceipt(receipt)
}

func (b *Broker) begin(ctx context.Context, operation Operation) (Receipt, bool, error) {
	if err := operation.Validate(); err != nil {
		return Receipt{}, false, err
	}
	if !operation.ExpiresAt.After(b.clock()) {
		return Receipt{}, false, ErrOperationExpired
	}
	existing, err := b.journal.FindByIdempotencyKey(ctx, operation.IdempotencyKey)
	if err == nil {
		if existing.Digest() != operation.Digest() {
			return Receipt{}, false, ErrIdempotencyConflict
		}
		receipt, receiptErr := b.journal.GetReceipt(ctx, existing.ID)
		if receiptErr == nil {
			return receipt, true, nil
		}
		if errors.Is(receiptErr, ErrNotFound) {
			return Receipt{}, false, ReconciliationRequiredError{OperationID: existing.ID}
		}
		return Receipt{}, false, receiptErr
	}
	if !errors.Is(err, ErrNotFound) {
		return Receipt{}, false, err
	}
	if err := b.journal.CreateIntent(ctx, operation); err != nil {
		return Receipt{}, false, err
	}
	return Receipt{ID: operation.ID + "-receipt", OperationID: operation.ID, OperationDigest: operation.DigestHex(), IdempotencyKey: operation.IdempotencyKey, Kind: operation.Kind, ExpectedOldSHA: operation.ExpectedOldSHA, TargetRef: operation.TargetRef, StartedAt: b.clock().UTC(), ClaimEpoch: operation.ClaimEpoch, ControllerEpoch: operation.ControllerEpoch}, false, nil
}

func (b *Broker) successReceipt(operation Operation, receipt Receipt, before, after, provider string, result bridgegit.RunResult) Receipt {
	receipt.Status, receipt.BeforeSHA, receipt.AfterSHA, receipt.ProviderResponseID = ReceiptSucceeded, before, after, provider
	receipt.FinishedAt, receipt.OutputDigest = b.clock().UTC(), resultDigest(result)
	return receipt
}

func (b *Broker) finishNoop(ctx context.Context, operation Operation, receipt Receipt, head string) (Receipt, error) {
	receipt = b.successReceipt(operation, receipt, head, head, "", bridgegit.RunResult{})
	return receipt, b.saveReceipt(receipt)
}

func (b *Broker) failed(ctx context.Context, operation Operation, receipt Receipt, cause error) error {
	receipt.Status, receipt.ErrorCode, receipt.FinishedAt = ReceiptFailed, errorCode(cause), b.clock().UTC()
	if errors.Is(cause, ErrReconciliationRequired) {
		receipt.Status, receipt.Reconciliation = ReceiptReconcile, true
	}
	if signErr := b.saveReceipt(receipt); signErr != nil {
		return signErr
	}
	return cause
}

func (b *Broker) saveReceipt(receipt Receipt) error {
	signature, err := b.signer.Sign(receipt.SigningBytes())
	if err != nil {
		return err
	}
	receipt.Signature = signature
	return b.journal.SaveReceipt(context.Background(), receipt)
}

func (b *Broker) prepare(ctx context.Context, operation Operation, credentialRef string) (RepositoryConfig, string, GitExecutor, func(), error) {
	repo, err := b.registry.Resolve(operation.RepositoryID)
	if err != nil {
		return RepositoryConfig{}, "", nil, func() {}, err
	}
	worktree := repo.CheckoutPath
	if operation.WorktreeID != "" {
		worktree = filepath.Join(repo.WorktreeRoot, operation.WorktreeID)
		rel, relErr := filepath.Rel(repo.WorktreeRoot, worktree)
		if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return RepositoryConfig{}, "", nil, func() {}, ErrUnsafeConfiguration
		}
	}
	if err := pathExistsAsDirectory(worktree); err != nil {
		return RepositoryConfig{}, "", nil, func() {}, err
	}
	credentialRef = strings.TrimSpace(credentialRef)
	if credentialRef == "" {
		credentialRef = repo.CredentialRef
	}
	executor, cleanup, err := b.executor(ctx, credentialRef)
	if err != nil {
		return RepositoryConfig{}, "", nil, func() {}, err
	}
	if err := b.inspect(ctx, executor, worktree, repo); err != nil {
		cleanup()
		return RepositoryConfig{}, "", nil, func() {}, err
	}
	return repo, worktree, executor, cleanup, nil
}

func (b *Broker) executor(ctx context.Context, reference string) (GitExecutor, func(), error) {
	credential, err := b.credential(ctx, reference)
	if err != nil {
		return nil, func() {}, err
	}
	env, cleanup, err := b.environment.WithCredential(credential)
	if err != nil {
		return nil, func() {}, err
	}
	switch value := b.git.(type) {
	case bridgegit.Runner:
		value.Environment = env
		return value, cleanup, nil
	case *bridgegit.Runner:
		copyValue := *value
		copyValue.Environment = env
		return copyValue, cleanup, nil
	default:
		if reference != "" {
			cleanup()
			return nil, func() {}, ErrCredentialUnavailable
		}
		return b.git, cleanup, nil
	}
}

func (b *Broker) credential(ctx context.Context, reference string) (Credential, error) {
	if reference == "" {
		return Credential{}, nil
	}
	if b.credentials == nil {
		return Credential{}, ErrCredentialUnavailable
	}
	return b.credentials.Get(ctx, reference)
}

func (b *Broker) inspect(ctx context.Context, executor GitExecutor, worktree string, repo RepositoryConfig) error {
	config, err := executor.Run(ctx, worktree, "config", "--local", "--no-includes", "--name-only", "--null", "--list")
	if err != nil {
		return ErrUnsafeConfiguration
	}
	for _, key := range strings.Split(config.Stdout, "\x00") {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		if unsafeGitKey(key) {
			return ErrUnsafeConfiguration
		}
	}
	remote, err := executor.Run(ctx, worktree, "remote", "get-url", repo.RemoteName)
	if err != nil || !sameRemoteURL(repo.RemoteURL, strings.TrimSpace(remote.Stdout)) {
		return ErrUnregisteredRemote
	}
	if info, err := os.Lstat(filepath.Join(worktree, ".gitmodules")); err == nil && info.Mode().IsRegular() {
		return ErrUnsafeConfiguration
	}
	return nil
}

func unsafeGitKey(key string) bool {
	if strings.HasPrefix(key, "remote.") {
		parts := strings.Split(key, ".")
		return len(parts) < 3 || parts[len(parts)-1] != "url" && parts[len(parts)-1] != "fetch"
	}
	for _, prefix := range []string{"alias.", "credential.", "filter.", "include", "url.", "submodule.", "core.hookspath", "core.fsmonitor", "core.sshcommand", "core.gitproxy", "core.attributesfile", "core.excludesfile", "diff.", "merge.", "pager.", "gpg.", "commit.gpgsign", "remote.", "protocol."} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (b *Broker) head(ctx context.Context, executor GitExecutor, worktree string) (string, error) {
	result, err := executor.Run(ctx, worktree, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	value := strings.TrimSpace(result.Stdout)
	if err != nil || !validGitObjectID(value) {
		return "", ErrDiverged
	}
	return strings.ToLower(value), nil
}

func (b *Broker) remoteSHA(ctx context.Context, executor GitExecutor, worktree, remote, ref string) (string, error) {
	result, err := executor.Run(ctx, worktree, "ls-remote", "--refs", remote, ref)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) == 0 {
		return strings.Repeat("0", 40), nil
	}
	if len(fields) < 2 || fields[1] != ref || !validGitObjectID(fields[0]) {
		return "", ErrDiverged
	}
	return strings.ToLower(fields[0]), nil
}

func (b *Broker) scan(ctx context.Context, worktree string, paths []string) error {
	if b.scanner == nil {
		return ErrPublicationBlocked
	}
	_, err := b.scanner.Scan(ctx, secretscan.Input{Root: worktree, Paths: paths})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPublicationBlocked, err)
	}
	return nil
}

func resultDigest(result bridgegit.RunResult) string {
	digest := sha256.Sum256([]byte(result.Stdout + "\x00" + result.Stderr))
	return fmt.Sprintf("%x", digest[:])
}

func errorCode(err error) string {
	if errors.Is(err, ErrReconciliationRequired) {
		return "reconciliation_required"
	}
	if errors.Is(err, ErrPublicationBlocked) {
		return "secret_scan_blocked"
	}
	if errors.Is(err, ErrUnsupportedHost) {
		return "unsupported_host"
	}
	if errors.Is(err, ErrDiverged) {
		return "diverged"
	}
	return "operation_failed"
}

var _ GitExecutor = bridgegit.Runner{}
