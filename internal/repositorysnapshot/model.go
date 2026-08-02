// Package repositorysnapshot inspects a bounded selection of committed Git
// objects without checking out or executing repository content.
package repositorysnapshot

import (
	"context"
	"errors"
	"time"
)

const (
	DetectorVersion = "1"

	MaxTreeEntries       = 20_000
	MaxTreeOutputBytes   = 2 << 20
	MaxSelectedBlobs     = 64
	MaxBlobBytes         = 128 << 10
	MaxTotalBlobBytes    = 1 << 20
	MaxObservations      = 1_024
	MaxLimitations       = 128
	MaxGitCommandOutput  = MaxTreeOutputBytes
	RepositorySnapshotV1 = "repository-snapshot-v1"
)

var (
	ErrInvalidRequest = errors.New("repositorysnapshot: invalid request")
	ErrNotConfigured  = errors.New("repositorysnapshot: repository profile is not configured")
	ErrRefNotAllowed  = errors.New("repositorysnapshot: requested ref is not allowed")
	ErrScopeNotFound  = errors.New("repositorysnapshot: scoped root is not a committed directory")
	ErrBoundsExceeded = errors.New("repositorysnapshot: inspection bounds exceeded")
	ErrConflict       = errors.New("repositorysnapshot: idempotency conflict")
)

type Request struct {
	RepositoryProfileID string `json:"repository_profile_id"`
	RequestedRef        string `json:"requested_ref"`
	ScopedRoot          string `json:"scoped_root"`
	AnalyzerVersion     string `json:"analyzer_version"`
	IdempotencyKey      string `json:"idempotency_key"`
}

type RepositoryIdentity struct {
	ProfileID string `json:"profile_id"`
}

type RefMetadata struct {
	Requested  string `json:"requested"`
	Kind       string `json:"kind"`
	AllowedRef string `json:"allowed_ref,omitempty"`
}

type Detector struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Observation struct {
	DetectorID      string `json:"detector_id"`
	DetectorVersion string `json:"detector_version"`
	EvidencePath    string `json:"evidence_path"`
	Observation     string `json:"observation"`
}

type Limitation struct {
	Code         string `json:"code"`
	EvidencePath string `json:"evidence_path,omitempty"`
}

type Bounds struct {
	TreeEntries        int `json:"tree_entries"`
	SelectedBlobs      int `json:"selected_blobs"`
	SelectedBlobBytes  int `json:"selected_blob_bytes"`
	MaxTreeEntries     int `json:"max_tree_entries"`
	MaxSelectedBlobs   int `json:"max_selected_blobs"`
	MaxBlobBytes       int `json:"max_blob_bytes"`
	MaxTotalBlobBytes  int `json:"max_total_blob_bytes"`
	MaxObservations    int `json:"max_observations"`
	MaxLimitations     int `json:"max_limitations"`
	MaxTreeOutputBytes int `json:"max_tree_output_bytes"`
}

type Response struct {
	ContractVersion string             `json:"contract_version"`
	OperationID     string             `json:"operation_id"`
	Repository      RepositoryIdentity `json:"repository"`
	ExactCommitSHA  string             `json:"exact_commit_sha"`
	Ref             RefMetadata        `json:"ref"`
	ScopedRoot      string             `json:"scoped_root"`
	AnalyzerVersion string             `json:"analyzer_version"`
	Detectors       []Detector         `json:"detectors"`
	Observations    []Observation      `json:"observations"`
	Limitations     []Limitation       `json:"limitations"`
	Bounds          Bounds             `json:"bounds"`
	ResultDigest    string             `json:"result_digest"`
}

// ConfiguredRepository is resolved from AgentBridge's active configured
// repository catalog. CheckoutPath is deliberately process-internal and has no
// JSON representation.
type ConfiguredRepository struct {
	ProfileID    string `json:"profile_id"`
	CheckoutPath string `json:"-"`
	Remote       string `json:"-"`
	AllowedRef   string `json:"-"`
}

type Catalog interface {
	ResolveRepositoryProfile(context.Context, string) (ConfiguredRepository, error)
}

type Operation struct {
	ID                  string
	IdempotencyKey      string
	RepositoryProfileID string
	RequestedRef        string
	ScopedRoot          string
	AnalyzerVersion     string
	RequestDigest       string
	ExactCommitSHA      string
	ResultDigest        string
	Status              string
	Response            Response
	RequestedAt         time.Time
	CompletedAt         time.Time
}

type Store interface {
	LoadRepositorySnapshot(context.Context, string) (Operation, error)
	LoadRepositorySnapshotByID(context.Context, string) (Operation, error)
	SaveRepositorySnapshot(context.Context, Operation) error
}
