package localcontrol

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

// Provider observations that end a turn. Everything else is streamed evidence.
const (
	decisionCompleted = "completed"
	decisionError     = "error"
)

type localDecision struct {
	taskID string
	kind   string
	detail string
}

// StartProgress runs the decision pump for locally executed tasks.
//
// Decisions are drained on their own goroutine rather than applied inside the
// runtime sink, because Start holds the command lock across the provider spawn:
// transitioning from the sink would stall the provider's event channel until
// Start returned. The pump also serialises decisions, so two observations for
// one task cannot race on the task revision.
func (s *Service) StartProgress(ctx context.Context) {
	if s == nil {
		return
	}
	s.progressOnce.Do(func() {
		decisions := make(chan localDecision, 128)
		s.progressMu.Lock()
		s.decisions = decisions
		s.progressMu.Unlock()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case decision := <-decisions:
					s.applyDecision(context.WithoutCancel(ctx), decision)
				}
			}
		}()
	})
}

func (s *Service) enqueueDecision(decision localDecision) error {
	s.progressMu.Lock()
	decisions := s.decisions
	s.progressMu.Unlock()
	if decisions == nil {
		return ErrNotConfigured
	}
	// Block rather than drop: the evidence is already durable, so backpressure
	// only slows the provider, whereas dropping would leave a finished bee
	// reported as still working.
	decisions <- decision
	return nil
}

// applyDecision advances a task that its provider has finished with. A verified
// turn stops at verification: gathering evidence runs the repository's own
// configured checks inside the isolated worktree, while committing and pushing
// act on the world and stay behind an explicit keeper action.
func (s *Service) applyDecision(ctx context.Context, decision localDecision) {
	view, err := s.taskView(ctx, decision.taskID)
	if err != nil {
		return
	}
	switch decision.kind {
	case decisionCompleted:
		if view.State != workmodel.Running {
			return
		}
		// Verify transitions running -> verifying and records the receipt. A
		// deterministic key keeps a replayed observation from verifying twice.
		_, _ = s.Verify(ctx, VerifyRequest{
			TaskID: view.ID, Revision: view.Revision,
			IdempotencyKey: "auto-verify-" + view.ExecutionID,
		})
	case decisionError:
		s.failFromProvider(ctx, view, decision.detail)
	}
}

func (s *Service) failFromProvider(ctx context.Context, view TaskView, detail string) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	current, err := s.taskView(ctx, view.ID)
	if err != nil || current.State == workmodel.Failed || current.State == workmodel.Canceled {
		return
	}
	_, _, _ = s.transition(ctx, current, workmodel.Failed, "provider_failed",
		map[string]any{"message": strings.TrimSpace(detail)})
}

// providerDecision reports the decision an observation carries, if any.
func providerDecision(providerType string) string {
	switch strings.TrimSpace(providerType) {
	case decisionCompleted:
		return decisionCompleted
	case decisionError:
		return decisionError
	}
	return ""
}

// decisionDetail extracts the provider's own message. provider.Event carries no
// JSON tags, so the durable payload uses Go field names.
func decisionDetail(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var decoded struct {
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ""
	}
	return decoded.Message
}
