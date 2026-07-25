package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var (
	ErrApprovalViaMCP   = errors.New("Claude approvals are resolved through MCP")
	ErrUsageUnavailable = errors.New("Claude usage has not been captured yet")
)

type SessionSink interface {
	SaveSession(context.Context, provider.Session) error
}
type AuthChecker func(context.Context) (provider.AuthStatus, error)
type TaskScope struct {
	ControlSocket string
	Capability    []byte
	Revoke        func()
}
type ScopeFactory func(provider.ID) (TaskScope, error)

type AdapterConfig struct {
	Spawn    Spawner
	Process  ProcessConfig
	Sessions SessionSink
	Usage    *UsageCache
	Auth     AuthChecker
	Scope    ScopeFactory
}

type runnerState struct {
	runner Runner
	revoke func()
}

type Adapter struct {
	spawn    Spawner
	process  ProcessConfig
	sessions SessionSink
	usage    *UsageCache
	auth     AuthChecker
	scope    ScopeFactory
	mu       sync.Mutex
	runners  map[string]runnerState
}

func NewAdapter(cfg AdapterConfig) *Adapter {
	if cfg.Spawn == nil {
		cfg.Spawn = OSSpawner{}
	}
	if cfg.Usage == nil {
		cfg.Usage = NewUsageCache()
	}
	return &Adapter{spawn: cfg.Spawn, process: cfg.Process, sessions: cfg.Sessions, usage: cfg.Usage, auth: cfg.Auth, scope: cfg.Scope, runners: make(map[string]runnerState)}
}

func (a *Adapter) Name() workmodel.Provider { return workmodel.ClaudeSubscription }

func (a *Adapter) Start(ctx context.Context, request provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	profile := claudeExecutionProfile(request.ExecutionProfile, request.Model)
	return a.start(ctx, request.TaskID, request.Input, "", profile)
}

func (a *Adapter) Resume(ctx context.Context, request provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	profile := claudeExecutionProfile(request.ExecutionProfile, request.Model)
	resume := request.Session.ExternalID
	if resume == "" {
		resume = request.Session.ID.String()
	}
	return a.start(ctx, request.TaskID, request.Input, resume, profile)
}

func claudeExecutionProfile(profile workmodel.ExecutionProfile, legacyModel string) workmodel.ExecutionProfile {
	profile.Model = strings.TrimSpace(profile.Model)
	profile.ReasoningEffort = strings.TrimSpace(profile.ReasoningEffort)
	profile.ApprovalMode = strings.TrimSpace(profile.ApprovalMode)
	if profile.Model == "" {
		profile.Model = strings.TrimSpace(legacyModel)
	}
	return profile
}

func (a *Adapter) start(ctx context.Context, taskID provider.ID, input provider.Input, resume string, profile workmodel.ExecutionProfile) (provider.Session, <-chan provider.Event, error) {
	cfg := a.process
	cfg.TaskID, cfg.InitialInput, cfg.ResumeSession = taskID, input, resume
	// An empty model means the operator chose nothing, which is the configured
	// default. A resume passes the model the session was dispatched with, so a
	// restart cannot quietly move a session onto a different model.
	if profile.Model != "" {
		cfg.Model = profile.Model
	}
	cfg.ReasoningEffort = profile.ReasoningEffort
	cfg.ApprovalMode = profile.ApprovalMode
	revoke := func() {}
	if a.scope != nil {
		scope, err := a.scope(taskID)
		if err != nil {
			return provider.Session{}, nil, err
		}
		cfg.ControlSocket, cfg.Capability = scope.ControlSocket, append([]byte(nil), scope.Capability...)
		var once sync.Once
		revoke = func() {
			once.Do(func() {
				if scope.Revoke != nil {
					scope.Revoke()
				}
				clear(cfg.Capability)
			})
		}
	}
	runner, err := a.spawn.Spawn(ctx, cfg)
	if err != nil {
		revoke()
		return provider.Session{}, nil, err
	}
	id, err := provider.NewID(runner.SessionID())
	if err != nil {
		_ = runner.Close()
		revoke()
		return provider.Session{}, nil, err
	}
	session := provider.Session{ID: id, TaskID: taskID, ExternalID: runner.SessionID(), Provider: workmodel.ClaudeSubscription}
	if a.sessions != nil {
		if err := a.sessions.SaveSession(ctx, session); err != nil {
			_ = runner.Close()
			revoke()
			return provider.Session{}, nil, fmt.Errorf("persist Claude session: %w", err)
		}
	}
	a.mu.Lock()
	a.runners[session.ExternalID] = runnerState{runner: runner, revoke: revoke}
	a.mu.Unlock()
	return session, observedEvents(runner.Events(), revoke), nil
}

func (a *Adapter) Steer(ctx context.Context, session provider.Session, input provider.Input) error {
	state, err := a.runner(session)
	if err != nil {
		return err
	}
	return state.runner.Send(ctx, input)
}

func (a *Adapter) Interrupt(_ context.Context, session provider.Session) error {
	state, err := a.runner(session)
	if err != nil {
		return err
	}
	err = state.runner.Close()
	state.revoke()
	a.mu.Lock()
	delete(a.runners, session.ExternalID)
	a.mu.Unlock()
	return err
}

func (a *Adapter) ResolveApproval(context.Context, provider.ApprovalDecision) error {
	return ErrApprovalViaMCP
}
func (a *Adapter) Usage(context.Context) (provider.Usage, error) { return a.usage.ProviderUsage() }
func (a *Adapter) AuthStatus(ctx context.Context) (provider.AuthStatus, error) {
	if a.auth == nil {
		return provider.AuthStatus{CheckedAt: time.Now().UTC()}, nil
	}
	return a.auth(ctx)
}

func (a *Adapter) runner(session provider.Session) (runnerState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	runner := a.runners[session.ExternalID]
	if runner.runner == nil {
		return runnerState{}, errors.New("unknown Claude session")
	}
	return runner, nil
}

func observedEvents(source <-chan provider.Event, revoke func()) <-chan provider.Event {
	output := make(chan provider.Event, 32)
	go func() {
		defer close(output)
		for event := range source {
			output <- event
			switch event.Type {
			case provider.EventCompleted, provider.EventAuthRequired, provider.EventError:
				revoke()
				return
			}
		}
		revoke()
	}()
	return output
}

var _ provider.Provider = (*Adapter)(nil)
