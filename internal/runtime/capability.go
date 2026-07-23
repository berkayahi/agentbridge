// Package runtime defines the versioned runtime adapter boundary.
package runtime

import "time"

type Model struct{ ID string }
type ReasoningProfile struct{ ID string }
type ApprovalMode string

const (
	ApprovalAskEveryTime     ApprovalMode = "ask_every_time"
	ApprovalProviderDefault  ApprovalMode = "provider_default"
	ApprovalAutoWithinPolicy ApprovalMode = "auto_within_policy"
)

type Capabilities struct {
	RuntimeVersion      string
	ObservedAt          time.Time
	Start, Resume       bool
	Steer, Interrupt    bool
	Close, Fork         bool
	Approvals, Usage    bool
	AuthRecovery        bool
	Models              []Model
	ReasoningProfiles   []ReasoningProfile
	NativeApprovalModes []ApprovalMode
}

type Installation struct {
	ID      string
	Version string
	Path    string
}
