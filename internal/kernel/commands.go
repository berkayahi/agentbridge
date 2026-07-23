package kernel

import (
	"time"

	"github.com/berkayahi/agentbridge/internal/execution"
)

type StartExecution struct {
	CommandID      string
	ExecutionID    string
	TaskID         string
	SessionID      string
	RepositoryID   string
	RuntimeID      string
	Model          string
	PolicySnapshot []byte
	FencingEpoch   uint64
	Input          Input
	ExpiresAt      time.Time
}

type ResumeExecution struct {
	CommandID, ExecutionID, TaskID, SessionID, RuntimeID string
	Input                                                Input
}

type SteerExecution struct {
	CommandID, ExecutionID, TaskID, RuntimeID string
	Input                                     Input
}

type CancelExecution struct {
	CommandID, ExecutionID, TaskID, RuntimeID string
}

type CloseExecution struct {
	CommandID, ExecutionID, TaskID, RuntimeID string
}

type ForkExecution struct {
	CommandID, ExecutionID, TaskID, RuntimeID, SuccessorTaskID string
	Input                                                      Input
}

type ApprovalDecision struct {
	CommandID, ExecutionID, TaskID, RuntimeID, ApprovalID string
	OperationDigest, PolicyDigest, AuthEvidenceRef, Nonce string
	Effect                                                string
	Allow                                                 bool
}

type RetryExecution struct {
	ExecutionID  string
	CommandID    string
	EvidenceID   string
	Basis        execution.RetryBasis
	FencingEpoch uint64
	IssuedAt     time.Time
}

type SuccessorExecution struct {
	ExecutionID  string
	NewID        string
	CommandID    string
	FencingEpoch uint64
	CreatedAt    time.Time
}

type CommandResult struct {
	CommandID string
	State     string
	Message   string
}
