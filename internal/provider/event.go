package provider

import "time"

type EventType string

const (
	EventAssistantMessage EventType = "assistant_message"
	EventCommandStarted   EventType = "command_started"
	EventCommandEnded     EventType = "command_ended"
	EventFileStarted      EventType = "file_started"
	EventFileEnded        EventType = "file_ended"
	EventToolStarted      EventType = "tool_started"
	EventToolEnded        EventType = "tool_ended"
	EventApprovalRequired EventType = "approval_required"
	EventApprovalExpired  EventType = "approval_expired"
	EventAuthRequired     EventType = "auth_required"
	EventRateLimited      EventType = "rate_limited"
	EventUsage            EventType = "usage"
	EventHeartbeat        EventType = "heartbeat"
	EventError            EventType = "error"
	EventCompleted        EventType = "completed"
)

// Event contains observable provider output only. Hidden reasoning is neither
// requested from providers nor represented by this contract.
type Event struct {
	ID        ID
	TaskID    ID
	RequestID ID
	Type      EventType
	Message   string
	Tool      string
	Path      string
	ExitCode  *int
	Usage     *Usage
	ResetAt   *time.Time
	CreatedAt time.Time
}
