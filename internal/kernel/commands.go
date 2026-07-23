package kernel

import "time"

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
	Allow                                                 bool
}

type CommandResult struct {
	CommandID string
	State     string
	Message   string
}
