package workmodel

// State is a durable task lifecycle state.
type State string

const (
	Queued           State = "queued"
	Preparing        State = "preparing"
	Running          State = "running"
	AwaitingApproval State = "awaiting_approval"
	AwaitingAuth     State = "awaiting_auth"
	Verifying        State = "verifying"
	Committing       State = "committing"
	Pushing          State = "pushing"
	Completed        State = "completed"
	Failed           State = "failed"
	Canceled         State = "canceled"
	Paused           State = "paused"
)

var transitions = map[State]map[State]struct{}{
	Queued:           stateSet(Preparing, Canceled, Paused),
	Preparing:        stateSet(Running, Paused, Failed, Canceled),
	Running:          stateSet(AwaitingApproval, AwaitingAuth, Verifying, Completed, Failed, Canceled, Paused),
	AwaitingApproval: stateSet(Running, Failed, Canceled, Paused),
	AwaitingAuth:     stateSet(Running, Paused, Canceled),
	Verifying:        stateSet(Committing, Failed, Canceled, Paused),
	Committing:       stateSet(Pushing, Failed, Paused),
	Pushing:          stateSet(Completed, Failed, Paused),
	Completed:        stateSet(Running),
	Failed:           stateSet(Queued),
	Canceled:         stateSet(),
	Paused:           stateSet(Queued),
}

func stateSet(states ...State) map[State]struct{} {
	set := make(map[State]struct{}, len(states))
	for _, state := range states {
		set[state] = struct{}{}
	}
	return set
}

// Valid reports whether s is a recognized lifecycle state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal reports whether a task in s can never transition again.
func (s State) Terminal() bool {
	return s == Completed || s == Canceled
}

// CanTransition reports whether moving from one valid state to another is approved.
func CanTransition(from, to State) bool {
	allowed, ok := transitions[from]
	if !ok || !to.Valid() {
		return false
	}
	_, ok = allowed[to]
	return ok
}
