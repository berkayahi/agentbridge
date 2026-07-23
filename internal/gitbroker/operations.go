// Package gitbroker defines the typed, fail-closed Git and hosting boundary.
package gitbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/gitref"
)

var (
	ErrInvalidOperation    = errors.New("gitbroker: invalid operation")
	ErrOperationExpired    = errors.New("gitbroker: operation expired")
	ErrOperationConflict   = errors.New("gitbroker: operation conflict")
	ErrPublicationBlocked  = errors.New("gitbroker: publication blocked")
	ErrDiverged            = errors.New("gitbroker: exact ref or base diverged")
	ErrUnregisteredRemote  = errors.New("gitbroker: unregistered remote")
	ErrUnsafeConfiguration = errors.New("gitbroker: unsafe Git configuration")
)

type Kind string

const (
	KindCheckpoint        Kind = "checkpoint"
	KindFetch             Kind = "fetch"
	KindCommit            Kind = "commit"
	KindPush              Kind = "push"
	KindReadPullRequest   Kind = "read_pull_request"
	KindCreatePullRequest Kind = "create_pull_request"
	KindSubmitReview      Kind = "submit_review"
	KindMerge             Kind = "merge"
)

func (k Kind) Valid() bool {
	switch k {
	case KindCheckpoint, KindFetch, KindCommit, KindPush, KindReadPullRequest,
		KindCreatePullRequest, KindSubmitReview, KindMerge:
		return true
	default:
		return false
	}
}

// Operation is the immutable intent persisted before a Git or hosting side
// effect. The broker never accepts raw argv or a caller-provided local path.
type Operation struct {
	ID              string
	OrganizationID  string
	Kind            Kind
	RepositoryID    string
	WorktreeID      string
	ExpectedBaseSHA string
	ExpectedOldSHA  string
	TargetRef       string
	IdempotencyKey  string
	ClaimEpoch      uint64
	ControllerEpoch uint64
	PolicyDigest    [32]byte
	ExpiresAt       time.Time
}

func NewOperation(value Operation) (Operation, error) {
	if err := value.Validate(); err != nil {
		return Operation{}, err
	}
	value.ExpiresAt = value.ExpiresAt.UTC()
	value.ExpectedBaseSHA = strings.ToLower(value.ExpectedBaseSHA)
	value.ExpectedOldSHA = strings.ToLower(value.ExpectedOldSHA)
	return value, nil
}

func (o Operation) Validate() error {
	if !validID(o.ID) || !validID(o.OrganizationID) || !o.Kind.Valid() || !validID(o.RepositoryID) ||
		(o.WorktreeID != "" && !validID(o.WorktreeID)) || !validID(o.IdempotencyKey) ||
		o.ClaimEpoch == 0 || o.ControllerEpoch == 0 || o.ExpiresAt.IsZero() {
		return ErrInvalidOperation
	}
	if o.TargetRef != "" && !gitref.Valid(o.TargetRef) {
		return ErrInvalidOperation
	}
	if o.ExpectedBaseSHA != "" && !validGitObjectID(o.ExpectedBaseSHA) {
		return ErrInvalidOperation
	}
	if o.ExpectedOldSHA == "" || (!validGitObjectID(o.ExpectedOldSHA) && !zeroObjectID(o.ExpectedOldSHA)) {
		return ErrInvalidOperation
	}
	return nil
}

func (o Operation) Digest() [32]byte {
	encoded, _ := json.Marshal(o)
	return sha256.Sum256(encoded)
}

func (o Operation) DigestHex() string {
	digest := o.Digest()
	return hex.EncodeToString(digest[:])
}

type CommitRequest struct {
	Operation     Operation
	Message       string
	ArtifactPaths []string
	CredentialRef string
}

type PushRequest struct {
	Operation     Operation
	CommitSHA     string
	ArtifactPaths []string
	CredentialRef string
}

type FetchRequest struct {
	Operation     Operation
	CredentialRef string
}

type CheckpointRequest struct {
	Operation       Operation
	Message         string
	ExpectedTreeSHA string
	ArtifactPaths   []string
}

func (r CommitRequest) Validate() error {
	if r.Operation.Kind != KindCommit || strings.TrimSpace(r.Message) == "" || strings.ContainsAny(r.Message, "\x00\r\n") {
		return ErrInvalidOperation
	}
	return nil
}

func (r PushRequest) Validate() error {
	if r.Operation.Kind != KindPush || !validGitObjectID(r.CommitSHA) {
		return ErrInvalidOperation
	}
	return nil
}

func (r FetchRequest) Validate() error {
	if r.Operation.Kind != KindFetch || r.Operation.TargetRef == "" {
		return ErrInvalidOperation
	}
	return nil
}

func (r CheckpointRequest) Validate() error {
	if r.Operation.Kind != KindCheckpoint || strings.TrimSpace(r.Message) == "" || strings.ContainsAny(r.Message, "\x00\r\n") {
		return ErrInvalidOperation
	}
	if r.ExpectedTreeSHA != "" && !validGitObjectID(r.ExpectedTreeSHA) {
		return ErrInvalidOperation
	}
	return nil
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func zeroObjectID(value string) bool {
	if !validGitObjectID(value) {
		return false
	}
	for _, r := range value {
		if r != '0' {
			return false
		}
	}
	return true
}
