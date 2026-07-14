// Package auth monitors subscription authentication and supervises operator-only
// login recovery without exposing provider credentials to ordinary transports.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
)

const (
	defaultCheckTimeout = 10 * time.Second
	defaultRecoveryTTL  = 10 * time.Minute
	maxTranscriptBytes  = 32 << 10
)

var (
	ErrCommandMissing   = errors.New("authentication command missing")
	ErrForbidden        = errors.New("recovery access forbidden")
	ErrNotFound         = errors.New("recovery session not found")
	ErrClosed           = errors.New("authentication service closed")
	ErrAuthUnhealthy    = errors.New("provider authentication is not healthy")
	ErrRecoveryActive   = errors.New("provider recovery is already active")
	ErrCodeSubmitted    = errors.New("recovery code was already submitted")
	ErrInvalidCode      = errors.New("invalid recovery code")
	ErrIncidentNotFound = errors.New("authentication incident not found")
)

type HealthKind string

const (
	HealthHealthy        HealthKind = "healthy"
	HealthExpired        HealthKind = "expired"
	HealthCommandMissing HealthKind = "command_missing"
	HealthTimeout        HealthKind = "timeout"
	HealthUnauthorized   HealthKind = "unauthorized"
	HealthUnknown        HealthKind = "unknown"
)

type Health struct {
	Provider  task.Provider
	Kind      HealthKind
	Message   string
	CheckedAt time.Time
}

type IncidentStatus string

const (
	IncidentOpen     IncidentStatus = "open"
	IncidentResolved IncidentStatus = "resolved"
)

// Incident contains durable metadata only. Provider output and recovery
// transcripts are intentionally absent.
type Incident struct {
	ID         string
	Provider   task.Provider
	Kind       HealthKind
	Status     IncidentStatus
	TaskIDs    []string
	OpenedAt   time.Time
	ResolvedAt *time.Time
}

// IncidentSummary is the only incident shape supplied to notification ports.
type IncidentSummary struct {
	Provider task.Provider
	Kind     HealthKind
	Affected int
	At       time.Time
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type TaskStore interface {
	NonterminalTasks(context.Context) ([]task.Task, error)
	Transition(context.Context, string, task.State, task.Event) error
}

type IncidentStore interface {
	SaveIncident(context.Context, Incident) error
	OpenIncident(context.Context, task.Provider) (Incident, error)
}

type Notifier interface {
	AuthIncident(context.Context, IncidentSummary) error
}

// Resumer owns provider-child restart and saved-session recovery. Validation
// must check the worktree, base revision, and provider session invariants.
type Resumer interface {
	ValidateResume(context.Context, task.Task) error
	ResumeTask(context.Context, task.Task) error
}

type RecoveryAuthorizer interface {
	AuthorizeRecovery(context.Context, string) error
}

type PTYRunner interface {
	Run(context.Context, string, []string, <-chan []byte, func([]byte)) error
}

type Options struct {
	Commands     CommandRunner
	Tasks        TaskStore
	Incidents    IncidentStore
	Notifier     Notifier
	Resumer      Resumer
	PTY          PTYRunner
	Authorizer   RecoveryAuthorizer
	Logger       *slog.Logger
	CheckTimeout time.Duration
	RecoveryTTL  time.Duration
	Now          func() time.Time
	NewID        func() string
}

type Service struct {
	commands     CommandRunner
	tasks        TaskStore
	incidents    IncidentStore
	notifier     Notifier
	resumer      Resumer
	pty          PTYRunner
	authorizer   RecoveryAuthorizer
	logger       *slog.Logger
	checkTimeout time.Duration
	recoveryTTL  time.Duration
	now          func() time.Time
	newID        func() string

	mu         sync.Mutex
	openingMu  sync.Mutex
	closed     bool
	affected   map[task.Provider][]task.Task
	open       map[task.Provider]Incident
	recoveries map[string]*recoverySession
	active     map[task.Provider]string
}

func NewService(options Options) (*Service, error) {
	if options.Commands == nil || options.Tasks == nil || options.Incidents == nil ||
		options.Notifier == nil || options.Resumer == nil || options.PTY == nil || options.Authorizer == nil {
		return nil, errors.New("auth: all ports are required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	if options.NewID == nil {
		options.NewID = randomID
	}
	if options.CheckTimeout <= 0 {
		options.CheckTimeout = defaultCheckTimeout
	}
	if options.RecoveryTTL <= 0 {
		options.RecoveryTTL = defaultRecoveryTTL
	}
	return &Service{
		commands: options.Commands, tasks: options.Tasks, incidents: options.Incidents,
		notifier: options.Notifier, resumer: options.Resumer, pty: options.PTY,
		authorizer: options.Authorizer, logger: options.Logger,
		checkTimeout: options.CheckTimeout, recoveryTTL: options.RecoveryTTL,
		now: options.Now, newID: options.NewID,
		affected: make(map[task.Provider][]task.Task), open: make(map[task.Provider]Incident),
		recoveries: make(map[string]*recoverySession),
		active:     make(map[task.Provider]string),
	}, nil
}

// Health runs the provider's local, non-turn-consuming subscription check.
func (s *Service) Health(ctx context.Context, provider task.Provider) Health {
	checkedAt := s.now()
	name, args, ok := statusCommand(provider)
	if !ok {
		return Health{Provider: provider, Kind: HealthUnknown, Message: "unsupported provider", CheckedAt: checkedAt}
	}
	checkCtx, cancel := context.WithTimeout(ctx, s.checkTimeout)
	defer cancel()
	output, err := s.commands.Run(checkCtx, name, args...)
	if checkErr := checkCtx.Err(); checkErr != nil {
		err = checkErr
	}
	return classifyHealth(provider, output, err, checkedAt)
}

func statusCommand(provider task.Provider) (string, []string, bool) {
	switch provider {
	case task.ProviderCodex:
		return "codex", []string{"login", "status"}, true
	case task.ProviderClaude:
		return "claude", []string{"auth", "status", "--json"}, true
	default:
		return "", nil, false
	}
}

func classifyHealth(provider task.Provider, output []byte, err error, at time.Time) Health {
	kind := HealthUnknown
	message := "authentication status could not be determined"
	lower := strings.ToLower(string(output) + " " + errorText(err))
	switch {
	case err == nil && authenticatedOutput(provider, output):
		kind, message = HealthHealthy, "subscription authentication is healthy"
	case errors.Is(err, context.DeadlineExceeded):
		kind, message = HealthTimeout, "authentication check timed out"
	case errors.Is(err, ErrCommandMissing) || isExecutableMissing(err):
		kind, message = HealthCommandMissing, "provider command is unavailable"
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized"):
		kind, message = HealthUnauthorized, "provider rejected subscription authentication"
	case provider == task.ProviderClaude && claudeLoggedOut(output):
		kind, message = HealthExpired, "subscription authentication requires login"
	case strings.Contains(lower, "expired") || strings.Contains(lower, "not logged") || strings.Contains(lower, "login required") || strings.Contains(lower, `"loggedin":false`):
		kind, message = HealthExpired, "subscription authentication requires login"
	}
	return Health{Provider: provider, Kind: kind, Message: message, CheckedAt: at}
}

func authenticatedOutput(provider task.Provider, output []byte) bool {
	switch provider {
	case task.ProviderCodex:
		return strings.Contains(strings.ToLower(string(output)), "logged in")
	case task.ProviderClaude:
		loggedIn, valid := claudeLoginStatus(output)
		return valid && loggedIn
	default:
		return false
	}
}

func claudeLoggedOut(output []byte) bool {
	loggedIn, valid := claudeLoginStatus(output)
	return valid && !loggedIn
}

func claudeLoginStatus(output []byte) (bool, bool) {
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(output, &status) != nil || status.LoggedIn == nil {
		return false, false
	}
	return *status.LoggedIn, true
}

func isExecutableMissing(err error) bool {
	var executableError *exec.Error
	return errors.As(err, &executableError) || errors.Is(err, os.ErrNotExist)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// CheckProvider performs a preflight or periodic check and opens an incident
// for every running task that depends on the unhealthy provider.
func (s *Service) CheckProvider(ctx context.Context, provider task.Provider) (Incident, error) {
	health := s.Health(ctx, provider)
	if err := ctx.Err(); err != nil {
		return Incident{}, err
	}
	if health.Kind == HealthHealthy {
		if err := s.resolveIfNoAffected(ctx, provider, health.CheckedAt); err != nil {
			return Incident{}, err
		}
		return Incident{}, nil
	}
	return s.openIncident(ctx, provider, health.Kind, health.CheckedAt)
}

// Monitor performs an immediate check and then checks on each interval until
// ctx is canceled. It is synchronous so its caller owns the goroutine lifetime.
func (s *Service) Monitor(ctx context.Context, interval time.Duration, providers ...task.Provider) error {
	if interval <= 0 {
		return errors.New("auth monitor interval must be positive")
	}
	if len(providers) == 0 {
		return errors.New("auth monitor requires a provider")
	}
	check := func() {
		for _, provider := range providers {
			if _, err := s.CheckProvider(ctx, provider); err != nil {
				if ctx.Err() == nil {
					s.logger.Error("authentication monitor check failed", "provider", provider)
				}
			}
		}
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			check()
		}
	}
}

// HandleProviderError maps runtime authentication failures onto the same
// durable incident flow used by preflight checks.
func (s *Service) HandleProviderError(ctx context.Context, provider task.Provider, providerErr error) (Incident, error) {
	health := classifyHealth(provider, nil, providerErr, s.now())
	if health.Kind != HealthUnauthorized && health.Kind != HealthExpired {
		return Incident{}, nil
	}
	return s.openIncident(ctx, provider, health.Kind, health.CheckedAt)
}

func (s *Service) openIncident(ctx context.Context, provider task.Provider, kind HealthKind, at time.Time) (Incident, error) {
	s.openingMu.Lock()
	defer s.openingMu.Unlock()
	if err := s.hydrateIncident(ctx, provider); err != nil {
		return Incident{}, err
	}
	s.mu.Lock()
	existing, alreadyOpen := s.open[provider]
	s.mu.Unlock()
	values, err := s.tasks.NonterminalTasks(ctx)
	if err != nil {
		return Incident{}, fmt.Errorf("list affected tasks: %w", err)
	}
	affected := make([]task.Task, 0)
	seen := make(map[string]struct{})
	for _, value := range values {
		if value.Provider == provider && value.State == task.AwaitingAuth {
			affected = append(affected, value)
			seen[value.ID] = struct{}{}
		}
	}
	var transitionErr error
	for _, value := range values {
		if value.Provider != provider || value.State != task.Running {
			continue
		}
		event := task.Event{
			ID: s.newID(), TaskID: value.ID, Type: task.EventAuthRequired,
			Visibility: task.VisibilityUser, Payload: safePayload(provider, kind), CreatedAt: at,
		}
		if err := s.tasks.Transition(ctx, value.ID, task.AwaitingAuth, event); err != nil {
			if transitionErr == nil {
				transitionErr = fmt.Errorf("await authentication for task %s: %w", value.ID, err)
			}
			continue
		}
		value.State = task.AwaitingAuth
		if _, ok := seen[value.ID]; !ok {
			affected = append(affected, value)
			seen[value.ID] = struct{}{}
		}
	}
	taskIDs := make([]string, len(affected))
	for i := range affected {
		taskIDs[i] = affected[i].ID
	}
	incident := existing
	if !alreadyOpen || existing.Status != IncidentOpen {
		incident = Incident{ID: s.newID(), Provider: provider, Kind: kind, Status: IncidentOpen, OpenedAt: at}
	}
	incident.TaskIDs = taskIDs
	if alreadyOpen && sameStrings(existing.TaskIDs, incident.TaskIDs) && transitionErr == nil {
		return existing, nil
	}
	if err := s.incidents.SaveIncident(ctx, incident); err != nil {
		return Incident{}, fmt.Errorf("save authentication incident: %w", err)
	}
	s.mu.Lock()
	s.affected[provider] = append([]task.Task(nil), affected...)
	s.open[provider] = incident
	s.mu.Unlock()
	if !alreadyOpen {
		if err := s.notifier.AuthIncident(ctx, IncidentSummary{Provider: provider, Kind: kind, Affected: len(affected), At: at}); err != nil {
			s.logger.Error("could not notify authentication incident", "provider", provider, "kind", kind)
		}
	}
	s.logger.Warn("provider authentication requires recovery", "provider", provider, "kind", kind, "affected", len(affected))
	if transitionErr != nil {
		return incident, transitionErr
	}
	return incident, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func safePayload(provider task.Provider, kind HealthKind) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Provider task.Provider `json:"provider"`
		Kind     HealthKind    `json:"kind"`
		Message  string        `json:"message"`
	}{provider, kind, "subscription authentication requires operator recovery"})
	return payload
}

type RecoveryStatus string

const (
	RecoveryRunning   RecoveryStatus = "running"
	RecoverySucceeded RecoveryStatus = "succeeded"
	RecoveryFailed    RecoveryStatus = "failed"
	RecoveryExpired   RecoveryStatus = "expired"
	RecoveryCanceled  RecoveryStatus = "canceled"
)

type RecoveryView struct {
	ID         string
	Provider   task.Provider
	Status     RecoveryStatus
	Transcript string
	StartedAt  time.Time
	FinishedAt *time.Time
}

type recoverySession struct {
	mu             sync.Mutex
	id             string
	provider       task.Provider
	status         RecoveryStatus
	transcript     []byte
	startedAt      time.Time
	finishedAt     *time.Time
	cancel         context.CancelFunc
	done           chan struct{}
	resultErr      error
	input          chan []byte
	submitted      bool
	acceptingInput bool
}

func (s *Service) StartRecovery(ctx context.Context, principal string, provider task.Provider) (string, error) {
	if err := s.authorizer.AuthorizeRecovery(ctx, principal); err != nil {
		return "", ErrForbidden
	}
	if err := s.hydrateIncident(ctx, provider); err != nil {
		return "", err
	}
	name, args, ok := loginCommand(provider)
	if !ok {
		return "", errors.New("unsupported provider")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", ErrClosed
	}
	if _, active := s.active[provider]; active {
		s.mu.Unlock()
		return "", ErrRecoveryActive
	}
	id := s.newID()
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.recoveryTTL)
	session := &recoverySession{
		id: id, provider: provider, status: RecoveryRunning, startedAt: s.now(),
		cancel: cancel, done: make(chan struct{}), input: make(chan []byte, 1), acceptingInput: true,
	}
	s.recoveries[id] = session
	s.active[provider] = id
	s.mu.Unlock()

	go s.runRecovery(runCtx, session, name, args)
	return id, nil
}

func loginCommand(provider task.Provider) (string, []string, bool) {
	switch provider {
	case task.ProviderCodex:
		return "codex", []string{"login", "--device-auth"}, true
	case task.ProviderClaude:
		return "claude", []string{"auth", "login", "--claudeai"}, true
	default:
		return "", nil, false
	}
}

func (s *Service) runRecovery(ctx context.Context, session *recoverySession, name string, args []string) {
	defer close(session.done)
	defer func() {
		s.mu.Lock()
		if s.active[session.provider] == session.id {
			delete(s.active, session.provider)
		}
		s.mu.Unlock()
	}()
	err := s.pty.Run(ctx, name, args, session.input, session.appendExpectedPrompt)
	session.mu.Lock()
	session.acceptingInput = false
	session.mu.Unlock()
	session.eraseInput()
	status := RecoveryFailed
	resultErr := err
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status, resultErr = RecoveryExpired, context.DeadlineExceeded
	} else if errors.Is(ctx.Err(), context.Canceled) {
		status, resultErr = RecoveryCanceled, context.Canceled
	} else if err == nil {
		health := s.Health(ctx, session.provider)
		if health.Kind == HealthHealthy {
			if resumeErr := s.resumeAffected(ctx, session.provider); resumeErr == nil {
				status, resultErr = RecoverySucceeded, nil
			} else {
				resultErr = resumeErr
			}
		} else {
			resultErr = ErrAuthUnhealthy
		}
	}
	finished := s.now()
	session.mu.Lock()
	clear(session.transcript)
	session.transcript = nil
	session.status = status
	session.finishedAt = &finished
	session.resultErr = resultErr
	session.mu.Unlock()
	if status == RecoverySucceeded {
		if err := s.resolveIncident(context.WithoutCancel(ctx), session.provider, finished); err != nil {
			s.logger.Error("could not resolve authentication incident", "provider", session.provider)
		}
	} else {
		pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.checkTimeout)
		defer cancel()
		if pauseErr := s.pauseAffected(pauseCtx, session.provider, "authentication recovery did not complete; manual review required"); pauseErr != nil {
			s.logger.Error("could not pause tasks after authentication recovery", "provider", session.provider)
		}
	}
}

func (s *Service) hydrateIncident(ctx context.Context, provider task.Provider) error {
	s.mu.Lock()
	_, ok := s.open[provider]
	s.mu.Unlock()
	if ok {
		return nil
	}
	incident, err := s.incidents.OpenIncident(ctx, provider)
	if errors.Is(err, ErrIncidentNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load authentication incident: %w", err)
	}
	s.mu.Lock()
	if _, exists := s.open[provider]; !exists {
		s.open[provider] = incident
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) resumeAffected(ctx context.Context, provider task.Provider) error {
	values, err := s.durableAffected(ctx, provider)
	if err != nil {
		return err
	}
	for _, value := range values {
		if err := s.resumer.ValidateResume(ctx, value); err != nil {
			if transitionErr := s.pauseTask(ctx, value, "saved task invariants changed; manual review required"); transitionErr != nil {
				return transitionErr
			}
			continue
		}
		if err := s.resumer.ResumeTask(ctx, value); err != nil {
			if transitionErr := s.pauseTask(ctx, value, "saved provider session could not be resumed safely"); transitionErr != nil {
				return transitionErr
			}
			continue
		}
		event := task.Event{ID: s.newID(), TaskID: value.ID, Type: task.EventStateTransitioned, Visibility: task.VisibilityUser, Payload: statePayload("subscription authentication restored"), CreatedAt: s.now()}
		if err := s.tasks.Transition(ctx, value.ID, task.Running, event); err != nil {
			return fmt.Errorf("resume task %s: %w", value.ID, err)
		}
	}
	return nil
}

func (s *Service) pauseAffected(ctx context.Context, provider task.Provider, reason string) error {
	values, err := s.durableAffected(ctx, provider)
	if err != nil {
		return err
	}
	for _, value := range values {
		if err := s.pauseTask(ctx, value, reason); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) durableAffected(ctx context.Context, provider task.Provider) ([]task.Task, error) {
	values, err := s.tasks.NonterminalTasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("reload authentication tasks: %w", err)
	}
	affected := make([]task.Task, 0)
	for _, value := range values {
		if value.Provider == provider && value.State == task.AwaitingAuth {
			affected = append(affected, value)
		}
	}
	return affected, nil
}

func (s *Service) pauseTask(ctx context.Context, value task.Task, reason string) error {
	event := task.Event{ID: s.newID(), TaskID: value.ID, Type: task.EventStateTransitioned, Visibility: task.VisibilityUser, Payload: statePayload(reason), CreatedAt: s.now()}
	if err := s.tasks.Transition(ctx, value.ID, task.Paused, event); err != nil {
		return fmt.Errorf("pause task %s: %w", value.ID, err)
	}
	return nil
}

func statePayload(reason string) json.RawMessage {
	payload, _ := json.Marshal(struct {
		Reason string `json:"reason"`
	}{reason})
	return payload
}

func (s *Service) resolveIncident(ctx context.Context, provider task.Provider, at time.Time) error {
	s.mu.Lock()
	incident, ok := s.open[provider]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	resolved := incident
	resolved.Status = IncidentResolved
	resolved.ResolvedAt = &at
	if err := s.incidents.SaveIncident(ctx, resolved); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.open, provider)
	delete(s.affected, provider)
	s.mu.Unlock()
	return nil
}

func (s *Service) resolveIfNoAffected(ctx context.Context, provider task.Provider, at time.Time) error {
	if err := s.hydrateIncident(ctx, provider); err != nil {
		return err
	}
	values, err := s.durableAffected(ctx, provider)
	if err != nil || len(values) != 0 {
		return err
	}
	return s.resolveIncident(ctx, provider, at)
}

func (session *recoverySession) appendExpectedPrompt(chunk []byte) {
	prompt := expectedPrompt(session.provider, string(chunk))
	if prompt == "" {
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	remaining := maxTranscriptBytes - len(session.transcript)
	if remaining <= 0 {
		return
	}
	if len(prompt) > remaining {
		prompt = prompt[:remaining]
	}
	session.transcript = append(session.transcript, prompt...)
}

// SubmitCode writes one operator-supplied callback code to the active PTY.
// The code is kept only in the bounded in-memory channel consumed by ExecPTY.
func (s *Service) SubmitCode(ctx context.Context, principal, id, code string) error {
	if err := s.authorizer.AuthorizeRecovery(ctx, principal); err != nil {
		return ErrForbidden
	}
	if strings.ContainsAny(code, "\r\n") {
		return ErrInvalidCode
	}
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 512 {
		return ErrInvalidCode
	}
	value := []byte(code + "\n")
	session, err := s.recovery(id)
	if err != nil {
		clear(value)
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.status != RecoveryRunning || !session.acceptingInput {
		clear(value)
		return ErrNotFound
	}
	if session.submitted {
		clear(value)
		return ErrCodeSubmitted
	}
	select {
	case <-ctx.Done():
		clear(value)
		return ctx.Err()
	case session.input <- value:
		session.submitted = true
		return nil
	default:
		clear(value)
		return ErrCodeSubmitted
	}
}

func (session *recoverySession) eraseInput() {
	for {
		select {
		case value := <-session.input:
			clear(value)
		default:
			return
		}
	}
}

func (s *Service) Recovery(ctx context.Context, principal, id string) (RecoveryView, error) {
	if err := s.authorizer.AuthorizeRecovery(ctx, principal); err != nil {
		return RecoveryView{}, ErrForbidden
	}
	session, err := s.recovery(id)
	if err != nil {
		return RecoveryView{}, err
	}
	return session.view(), nil
}

func (s *Service) CancelRecovery(ctx context.Context, principal, id string) error {
	if err := s.authorizer.AuthorizeRecovery(ctx, principal); err != nil {
		return ErrForbidden
	}
	session, err := s.recovery(id)
	if err != nil {
		return err
	}
	session.cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-session.done:
		return nil
	}
}

func (s *Service) WaitRecovery(ctx context.Context, id string) error {
	session, err := s.recovery(id)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-session.done:
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.resultErr
	}
}

func (s *Service) recovery(id string) (*recoverySession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.recoveries[id]
	if !ok {
		return nil, ErrNotFound
	}
	return session, nil
}

func (session *recoverySession) view() RecoveryView {
	session.mu.Lock()
	defer session.mu.Unlock()
	return RecoveryView{
		ID: session.id, Provider: session.provider, Status: session.status,
		Transcript: string(session.transcript), StartedAt: session.startedAt, FinishedAt: session.finishedAt,
	}
}

// Close cancels all recovery processes and waits for their goroutines to exit.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sessions := make([]*recoverySession, 0, len(s.recoveries))
	for _, session := range s.recoveries {
		sessions = append(sessions, session)
		session.cancel()
	}
	s.mu.Unlock()
	for _, session := range sessions {
		<-session.done
	}
	return nil
}

func randomID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(value[:])
}
