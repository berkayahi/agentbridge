package task

import "testing"

func TestTransition(t *testing.T) {
	valid := [][2]State{
		{Queued, Preparing}, {Queued, Canceled}, {Queued, Paused},
		{Preparing, Running}, {Preparing, Paused}, {Preparing, Failed}, {Preparing, Canceled},
		{Running, AwaitingApproval}, {Running, AwaitingAuth}, {Running, Verifying}, {Running, Failed}, {Running, Canceled}, {Running, Paused},
		{AwaitingApproval, Running}, {AwaitingApproval, Failed}, {AwaitingApproval, Canceled}, {AwaitingApproval, Paused},
		{AwaitingAuth, Running}, {AwaitingAuth, Paused}, {AwaitingAuth, Canceled},
		{Verifying, Committing}, {Verifying, Failed}, {Verifying, Canceled}, {Verifying, Paused},
		{Committing, Pushing}, {Committing, Failed}, {Committing, Paused},
		{Pushing, Completed}, {Pushing, Failed}, {Pushing, Paused},
		{Failed, Queued}, {Paused, Queued},
	}

	allowed := make(map[[2]State]bool, len(valid))
	for _, transition := range valid {
		allowed[transition] = true
	}

	states := []State{Queued, Preparing, Running, AwaitingApproval, AwaitingAuth, Verifying, Committing, Pushing, Completed, Failed, Canceled, Paused}
	for _, from := range states {
		for _, to := range states {
			want := allowed[[2]State{from, to}]
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				if got := CanTransition(from, to); got != want {
					t.Fatalf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
				}
			})
		}
	}
}

func TestTransitionRejectsUnknownStates(t *testing.T) {
	for _, test := range []struct {
		name string
		from State
		to   State
	}{
		{name: "unknown source", from: "unknown", to: Queued},
		{name: "unknown target", from: Queued, to: "unknown"},
		{name: "both unknown", from: "before", to: "after"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if CanTransition(test.from, test.to) {
				t.Fatalf("CanTransition(%q, %q) = true, want false", test.from, test.to)
			}
		})
	}
}

func TestStateValidityAndTerminality(t *testing.T) {
	if !Running.Valid() || State("unknown").Valid() {
		t.Fatal("state validity is incorrect")
	}
	if !Completed.Terminal() || !Canceled.Terminal() || Failed.Terminal() || Paused.Terminal() {
		t.Fatal("state terminality is incorrect")
	}
}
