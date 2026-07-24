package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const (
	defaultEventBuffer     = 128
	defaultApprovalTimeout = 10 * time.Minute
)

var (
	ErrApprovalNotPending = errors.New("approval is not pending")
	ErrApprovalRejected   = errors.New("approval decision rejected")
	ErrAPIKeyAccount      = errors.New("Codex API-key accounts are not supported")
)

type rpcTransport interface {
	Call(context.Context, string, any, any) error
	Notify(context.Context, string, any) error
	RespondResult(context.Context, json.RawMessage, any) error
	Notifications() <-chan ServerMessage
	Requests() <-chan ServerMessage
}

type SessionSink interface {
	SaveSession(context.Context, provider.Session) error
}

type ApprovalSink interface {
	SaveApproval(context.Context, ApprovalRequest) error
}

type ApprovalRequest struct {
	ID        provider.ID
	TaskID    provider.ID
	UserID    string
	Kind      string
	Summary   string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type AdapterConfig struct {
	Sessions        SessionSink
	Approvals       ApprovalSink
	ApprovalUser    func(provider.ID) string
	ApprovalTimeout time.Duration
	Now             func() time.Time
}

type sessionState struct {
	session provider.Session
	events  chan provider.Event
	turnID  string
}

type pendingApproval struct {
	request ApprovalRequest
	rpcID   json.RawMessage
	events  chan<- provider.Event
}

type Adapter struct {
	rpc             rpcTransport
	sessions        SessionSink
	approvals       ApprovalSink
	approvalUser    func(provider.ID) string
	approvalTimeout time.Duration
	now             func() time.Time

	mu        sync.Mutex
	threads   map[string]*sessionState
	pending   map[string]pendingApproval
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewAdapter(rpc rpcTransport, cfg AdapterConfig) *Adapter {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = defaultApprovalTimeout
	}
	if cfg.ApprovalUser == nil {
		cfg.ApprovalUser = func(provider.ID) string { return "" }
	}
	a := &Adapter{
		rpc: rpc, sessions: cfg.Sessions, approvals: cfg.Approvals,
		approvalUser: cfg.ApprovalUser, approvalTimeout: cfg.ApprovalTimeout, now: cfg.Now,
		threads: make(map[string]*sessionState), pending: make(map[string]pendingApproval), closed: make(chan struct{}),
	}
	a.wg.Add(1)
	go a.pump()
	return a
}

func (a *Adapter) Name() workmodel.Provider { return workmodel.CodexSubscription }

func (a *Adapter) Start(ctx context.Context, req provider.StartRequest) (provider.Session, <-chan provider.Event, error) {
	if err := req.Input.Validate(); err != nil {
		return provider.Session{}, nil, err
	}
	params := map[string]any{"experimentalRawEvents": false}
	if req.WorkingDirectory != "" {
		params["cwd"] = req.WorkingDirectory
	}
	if req.Model != "" {
		params["model"] = req.Model
	}
	var response threadResponse
	if err := a.rpc.Call(ctx, "thread/start", params, &response); err != nil {
		return provider.Session{}, nil, mapCallError(err)
	}
	session, err := newSession(req.TaskID, response.Thread.ID)
	if err != nil {
		return provider.Session{}, nil, err
	}
	if err := a.persistSession(ctx, session); err != nil {
		return provider.Session{}, nil, err
	}
	state := a.registerSession(session)
	if err := a.startTurn(ctx, state, req.Input); err != nil {
		return provider.Session{}, nil, err
	}
	return session, state.events, nil
}

func (a *Adapter) Resume(ctx context.Context, req provider.ResumeRequest) (provider.Session, <-chan provider.Event, error) {
	if err := req.Input.Validate(); err != nil {
		return provider.Session{}, nil, err
	}
	threadID := req.Session.ThreadID
	if threadID == "" {
		threadID = req.Session.ExternalID
	}
	var response threadResponse
	if err := a.rpc.Call(ctx, "thread/resume", map[string]any{"threadId": threadID}, &response); err != nil {
		return provider.Session{}, nil, mapCallError(err)
	}
	if response.Thread.ID != "" {
		threadID = response.Thread.ID
	}
	session := req.Session
	session.TaskID = req.TaskID
	session.ThreadID = threadID
	session.ExternalID = threadID
	session.Provider = workmodel.CodexSubscription
	if !session.ID.Valid() {
		id, err := provider.NewID(threadID)
		if err != nil {
			return provider.Session{}, nil, err
		}
		session.ID = id
	}
	if err := a.persistSession(ctx, session); err != nil {
		return provider.Session{}, nil, err
	}
	state := a.registerSession(session)
	if err := a.startTurn(ctx, state, req.Input); err != nil {
		return provider.Session{}, nil, err
	}
	return session, state.events, nil
}

func (a *Adapter) Steer(ctx context.Context, session provider.Session, input provider.Input) error {
	if err := input.Validate(); err != nil {
		return err
	}
	turnID, err := a.activeTurn(session.ThreadID)
	if err != nil {
		return err
	}
	var response turnResponse
	err = a.rpc.Call(ctx, "turn/steer", map[string]any{
		"threadId": session.ThreadID, "expectedTurnId": turnID, "input": codexInput(input),
	}, &response)
	if err != nil {
		return mapCallError(err)
	}
	if response.Turn.ID != "" {
		a.setTurn(session.ThreadID, response.Turn.ID)
	}
	return nil
}

func (a *Adapter) Interrupt(ctx context.Context, session provider.Session) error {
	turnID, err := a.activeTurn(session.ThreadID)
	if err != nil {
		return err
	}
	return mapCallError(a.rpc.Call(ctx, "turn/interrupt", map[string]any{"threadId": session.ThreadID, "turnId": turnID}, nil))
}

func (a *Adapter) ResolveApproval(ctx context.Context, decision provider.ApprovalDecision) error {
	key := decision.RequestID.String()
	a.mu.Lock()
	pending, ok := a.pending[key]
	if ok {
		delete(a.pending, key)
	}
	a.mu.Unlock()
	if !ok {
		return ErrApprovalNotPending
	}
	valid := decision.TaskID == pending.request.TaskID &&
		decision.UserID == pending.request.UserID &&
		a.now().Before(pending.request.ExpiresAt)
	response := "decline"
	if valid && decision.Allow {
		response = "accept"
	}
	if err := a.rpc.RespondResult(ctx, pending.rpcID, map[string]any{"decision": response}); err != nil {
		return err
	}
	if !valid {
		return ErrApprovalRejected
	}
	return nil
}

func (a *Adapter) Usage(ctx context.Context) (provider.Usage, error) {
	var rateLimits rateLimitsResponse
	if err := a.rpc.Call(ctx, "account/rateLimits/read", map[string]any{}, &rateLimits); err != nil {
		return provider.Usage{}, mapCallError(err)
	}
	var ignored json.RawMessage
	if err := a.rpc.Call(ctx, "account/usage/read", map[string]any{}, &ignored); err != nil {
		return provider.Usage{}, mapCallError(err)
	}
	usage := provider.Usage{Provider: workmodel.CodexSubscription, ObservedAt: a.now().UTC()}
	usage.Windows = appendWindow(usage.Windows, "primary", rateLimits.RateLimits.Primary)
	usage.Windows = appendWindow(usage.Windows, "secondary", rateLimits.RateLimits.Secondary)
	return usage, nil
}

func (a *Adapter) AuthStatus(ctx context.Context) (provider.AuthStatus, error) {
	var response accountResponse
	if err := a.rpc.Call(ctx, "account/read", map[string]any{}, &response); err != nil {
		return provider.AuthStatus{}, mapCallError(err)
	}
	if response.Account.Type == "apiKey" {
		return provider.AuthStatus{}, ErrAPIKeyAccount
	}
	return provider.AuthStatus{
		Authenticated: response.Account.Type == "chatgpt",
		Account:       response.Account.Email, CheckedAt: a.now().UTC(),
	}, nil
}

func (a *Adapter) Close() {
	a.closeOnce.Do(func() { close(a.closed) })
	a.wg.Wait()
}

func (a *Adapter) startTurn(ctx context.Context, state *sessionState, input provider.Input) error {
	var response turnResponse
	if err := a.rpc.Call(ctx, "turn/start", map[string]any{"threadId": state.session.ThreadID, "input": codexInput(input)}, &response); err != nil {
		return mapCallError(err)
	}
	a.setTurn(state.session.ThreadID, response.Turn.ID)
	return nil
}

func (a *Adapter) persistSession(ctx context.Context, session provider.Session) error {
	if a.sessions == nil {
		return nil
	}
	if err := a.sessions.SaveSession(ctx, session); err != nil {
		return fmt.Errorf("persist Codex session: %w", err)
	}
	return nil
}

func (a *Adapter) registerSession(session provider.Session) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state := a.threads[session.ThreadID]; state != nil {
		state.session = session
		return state
	}
	state := &sessionState{session: session, events: make(chan provider.Event, defaultEventBuffer)}
	a.threads[session.ThreadID] = state
	return state
}

func (a *Adapter) eventsFor(threadID string) <-chan provider.Event {
	state, err := a.state(threadID)
	if err != nil {
		closed := make(chan provider.Event)
		close(closed)
		return closed
	}
	return state.events
}

func (a *Adapter) state(threadID string) (*sessionState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.threads[threadID]
	if state == nil {
		return nil, fmt.Errorf("unknown Codex thread")
	}
	return state, nil
}

func (a *Adapter) activeTurn(threadID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.threads[threadID]
	if state == nil {
		return "", fmt.Errorf("unknown Codex thread")
	}
	return state.turnID, nil
}

func (a *Adapter) setTurn(threadID, turnID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if state := a.threads[threadID]; state != nil {
		state.turnID = turnID
	}
}

func (a *Adapter) pump() {
	defer a.wg.Done()
	notifications := a.rpc.Notifications()
	requests := a.rpc.Requests()
	for notifications != nil || requests != nil {
		select {
		case <-a.closed:
			return
		case message, ok := <-notifications:
			if !ok {
				notifications = nil
				continue
			}
			a.handleNotification(message)
		case message, ok := <-requests:
			if !ok {
				requests = nil
				continue
			}
			a.handleRequest(message)
		}
	}
}

func (a *Adapter) handleNotification(message ServerMessage) {
	threadID := extractThreadID(message.Params)
	state, err := a.state(threadID)
	if err != nil {
		return
	}
	event, ok := mapNotification(message, state.session.TaskID, a.now().UTC())
	if !ok {
		return
	}
	select {
	case state.events <- event:
	default:
	}
}

func (a *Adapter) handleRequest(message ServerMessage) {
	if message.Method != "item/commandExecution/requestApproval" && message.Method != "item/fileChange/requestApproval" {
		_ = a.rpc.RespondResult(context.Background(), responseID(message), map[string]any{"decision": "decline"})
		return
	}
	var params struct {
		ThreadID string `json:"threadId"`
		Command  string `json:"command"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal(message.Params, &params) != nil {
		_ = a.rpc.RespondResult(context.Background(), responseID(message), map[string]any{"decision": "decline"})
		return
	}
	state, err := a.state(params.ThreadID)
	if err != nil {
		_ = a.rpc.RespondResult(context.Background(), responseID(message), map[string]any{"decision": "decline"})
		return
	}
	now := a.now().UTC()
	summary := params.Command
	if summary == "" {
		summary = params.Reason
	}
	id, err := approvalID(state.session.TaskID, message.ID, summary, now)
	if err != nil {
		_ = a.rpc.RespondResult(context.Background(), responseID(message), map[string]any{"decision": "decline"})
		return
	}
	request := ApprovalRequest{
		ID: id, TaskID: state.session.TaskID, UserID: a.approvalUser(state.session.TaskID),
		Kind: message.Method, Summary: summary, CreatedAt: now, ExpiresAt: now.Add(a.approvalTimeout),
	}
	if a.approvals != nil {
		if err := a.approvals.SaveApproval(context.Background(), request); err != nil {
			_ = a.rpc.RespondResult(context.Background(), responseID(message), map[string]any{"decision": "decline"})
			return
		}
	}
	a.mu.Lock()
	a.pending[id.String()] = pendingApproval{request: request, rpcID: responseID(message), events: state.events}
	a.mu.Unlock()
	select {
	case state.events <- provider.Event{TaskID: request.TaskID, RequestID: request.ID, Type: provider.EventApprovalRequired, Message: request.Summary, CreatedAt: now}:
	default:
		a.mu.Lock()
		delete(a.pending, id.String())
		a.mu.Unlock()
		_ = a.rpc.RespondResult(context.Background(), responseID(message), map[string]any{"decision": "decline"})
		return
	}
	a.wg.Add(1)
	go a.expireApproval(id, a.approvalTimeout)
}

// approvalID scopes a provider request to its AgentBridge task. Codex request
// IDs are only unique within one app-server process and commonly restart at
// zero for every thread. Exposing that native ID directly would let a later
// task collide with a durable approval from an earlier task.
func approvalID(taskID provider.ID, nativeID, summary string, now time.Time) (provider.ID, error) {
	if !taskID.Valid() || strings.TrimSpace(nativeID) == "" || now.IsZero() {
		return provider.ID{}, provider.ErrInvalidInput
	}
	seed := strings.Join([]string{taskID.String(), nativeID, summary, now.UTC().Format(time.RFC3339Nano)}, "\x00")
	digest := sha256.Sum256([]byte(seed))
	return provider.NewID("approval-" + hex.EncodeToString(digest[:16]))
}

func (a *Adapter) expireApproval(id provider.ID, after time.Duration) {
	defer a.wg.Done()
	timer := time.NewTimer(after)
	defer timer.Stop()
	select {
	case <-a.closed:
		return
	case <-timer.C:
	}
	a.mu.Lock()
	pending, ok := a.pending[id.String()]
	if ok {
		delete(a.pending, id.String())
	}
	a.mu.Unlock()
	if ok {
		_ = a.rpc.RespondResult(context.Background(), pending.rpcID, map[string]any{"decision": "decline"})
		if pending.events != nil {
			select {
			case pending.events <- provider.Event{
				TaskID: pending.request.TaskID, RequestID: pending.request.ID,
				Type: provider.EventApprovalExpired, Message: "approval request expired", CreatedAt: a.now().UTC(),
			}:
			case <-a.closed:
			}
		}
	}
}

func responseID(message ServerMessage) json.RawMessage {
	if len(message.RawID) > 0 {
		return append(json.RawMessage(nil), message.RawID...)
	}
	encoded, _ := json.Marshal(message.ID)
	return encoded
}

func newSession(taskID provider.ID, threadID string) (provider.Session, error) {
	id, err := provider.NewID(threadID)
	if err != nil {
		return provider.Session{}, fmt.Errorf("invalid Codex thread id: %w", err)
	}
	return provider.Session{ID: id, TaskID: taskID, ExternalID: threadID, ThreadID: threadID, Provider: workmodel.CodexSubscription}, nil
}

func codexInput(input provider.Input) []map[string]any {
	items := make([]map[string]any, 0, 1+len(input.Attachments))
	if strings.TrimSpace(input.Text) != "" {
		items = append(items, map[string]any{"type": "text", "text": input.Text})
	}
	for _, attachment := range input.Attachments {
		items = append(items, map[string]any{"type": "localImage", "path": attachment.Path()})
	}
	return items
}

type threadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type turnResponse struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type rateLimitWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    *int64  `json:"resetsAt"`
}

type rateLimitsResponse struct {
	RateLimits struct {
		Primary   *rateLimitWindow `json:"primary"`
		Secondary *rateLimitWindow `json:"secondary"`
	} `json:"rateLimits"`
}

type accountResponse struct {
	RequiresOpenAIAuth bool `json:"requiresOpenaiAuth"`
	Account            struct {
		Type  string `json:"type"`
		Email string `json:"email"`
	} `json:"account"`
}

func appendWindow(windows []provider.UsageWindow, name string, window *rateLimitWindow) []provider.UsageWindow {
	if window == nil {
		return windows
	}
	result := provider.UsageWindow{Name: name, UsedPercent: window.UsedPercent}
	if window.ResetsAt != nil {
		result.ResetsAt = time.Unix(*window.ResetsAt, 0).UTC()
	}
	return append(windows, result)
}

func mapCallError(err error) error {
	if err == nil {
		return nil
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "login required") || strings.Contains(lower, "not logged in") {
		return fmt.Errorf("Codex authentication required: %w", err)
	}
	return err
}

var _ provider.Provider = (*Adapter)(nil)
