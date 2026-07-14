package task

import (
	"encoding/json"
	"time"
)

type EventVisibility string

const (
	VisibilityInternal EventVisibility = "internal"
	VisibilityUser     EventVisibility = "user"
)

type EventType string

const (
	EventTaskCreated       EventType = "task_created"
	EventStateTransitioned EventType = "state_transitioned"
	EventProviderMessage   EventType = "provider_message"
	EventApprovalRequested EventType = "approval_requested"
	EventApprovalResolved  EventType = "approval_resolved"
	EventAuthRequired      EventType = "auth_required"
	EventAttachmentAdded   EventType = "attachment_added"
	EventVerification      EventType = "verification"
	EventCommitCreated     EventType = "commit_created"
	EventPushCompleted     EventType = "push_completed"
	EventDeployment        EventType = "deployment"
	EventFailure           EventType = "failure"
)

type Event struct {
	ID              string
	TaskID          string
	Type            EventType
	Visibility      EventVisibility
	ProviderEventID string
	Payload         json.RawMessage
	CreatedAt       time.Time
}
