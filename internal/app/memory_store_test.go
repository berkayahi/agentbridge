package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
)

type memoryStore struct {
	mu           sync.Mutex
	tasks        map[string]task.Task
	events       map[string][]task.Event
	sessions     map[string]task.Session
	attachments  map[string][]task.Attachment
	approvals    map[string]task.Approval
	incidents    map[task.Provider]task.AuthIncident
	leases       map[string]store.Lease
	expiredErr   error
	heartbeatErr error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{tasks: map[string]task.Task{}, events: map[string][]task.Event{}, sessions: map[string]task.Session{}, attachments: map[string][]task.Attachment{}, approvals: map[string]task.Approval{}, incidents: map[task.Provider]task.AuthIncident{}, leases: map[string]store.Lease{}}
}
func (s *memoryStore) CreateTask(_ context.Context, value task.Task, event task.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[value.ID]; ok {
		return store.ErrConflict
	}
	s.tasks[value.ID] = value
	s.events[value.ID] = append(s.events[value.ID], event)
	return nil
}
func (s *memoryStore) Transition(_ context.Context, id string, state task.State, event task.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	if !task.CanTransition(value.State, state) {
		return store.ErrInvalidTransition
	}
	value.State, value.UpdatedAt = state, event.CreatedAt
	if state == task.Running && value.StartedAt == nil {
		at := event.CreatedAt
		value.StartedAt = &at
	}
	if state == task.Completed || state == task.Failed || state == task.Canceled {
		at := event.CreatedAt
		value.FinishedAt = &at
	}
	s.tasks[id] = value
	s.events[id] = append(s.events[id], event)
	return nil
}
func (s *memoryStore) AppendEvent(_ context.Context, value task.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[value.TaskID] = append(s.events[value.TaskID], value)
	return nil
}
func (s *memoryStore) Events(_ context.Context, id string) ([]task.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]task.Event(nil), s.events[id]...), nil
}
func (s *memoryStore) Task(_ context.Context, id string) (task.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return task.Task{}, store.ErrNotFound
	}
	return value, nil
}
func (s *memoryStore) ListTasks(_ context.Context, _ store.ListFilter) ([]task.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]task.Task, 0, len(s.tasks))
	for _, value := range s.tasks {
		values = append(values, value)
	}
	return values, nil
}
func (s *memoryStore) NonterminalTasks(_ context.Context) ([]task.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []task.Task
	for _, value := range s.tasks {
		if !value.State.Terminal() {
			values = append(values, value)
		}
	}
	return values, nil
}
func (s *memoryStore) SaveAttachment(_ context.Context, value task.Attachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attachments[value.TaskID] = append(s.attachments[value.TaskID], value)
	return nil
}
func (s *memoryStore) Attachments(_ context.Context, id string) ([]task.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]task.Attachment(nil), s.attachments[id]...), nil
}
func (s *memoryStore) UpsertSession(_ context.Context, value task.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[value.ID] = value
	return nil
}
func (s *memoryStore) ResumableSessions(_ context.Context) ([]task.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []task.Session
	for _, value := range s.sessions {
		if value.Resumable {
			values = append(values, value)
		}
	}
	return values, nil
}
func (s *memoryStore) UpsertApproval(_ context.Context, value task.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvals[value.ID] = value
	return nil
}
func (s *memoryStore) PendingApprovals(_ context.Context) ([]task.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var values []task.Approval
	for _, value := range s.approvals {
		if value.Status == task.ApprovalPending {
			values = append(values, value)
		}
	}
	return values, nil
}
func (s *memoryStore) UpsertAuthIncident(_ context.Context, value task.AuthIncident) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incidents[value.Provider] = value
	return nil
}
func (s *memoryStore) OpenAuthIncident(_ context.Context, p task.Provider) (task.AuthIncident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.incidents[p]
	if !ok {
		return task.AuthIncident{}, store.ErrNotFound
	}
	return value, nil
}
func (s *memoryStore) AcquireLease(_ context.Context, repo, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value, ok := s.leases[repo]; ok && value.OwnerID != owner && value.ExpiresAt.After(time.Now()) {
		return false, nil
	}
	now := time.Now()
	s.leases[repo] = store.Lease{RepoProfileID: repo, OwnerID: owner, AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(ttl)}
	return true, nil
}
func (s *memoryStore) HeartbeatLease(_ context.Context, repo, owner string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heartbeatErr != nil {
		return s.heartbeatErr
	}
	value, ok := s.leases[repo]
	if !ok || value.OwnerID != owner {
		return store.ErrConflict
	}
	value.ExpiresAt = time.Now().Add(ttl)
	s.leases[repo] = value
	return nil
}
func (s *memoryStore) ReleaseLease(_ context.Context, repo, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.leases[repo]
	if !ok || value.OwnerID != owner {
		return store.ErrConflict
	}
	delete(s.leases, repo)
	return nil
}
func (s *memoryStore) ExpiredLeases(_ context.Context) ([]store.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expiredErr != nil {
		return nil, s.expiredErr
	}
	var values []store.Lease
	for _, value := range s.leases {
		if !value.ExpiresAt.After(time.Now()) {
			values = append(values, value)
		}
	}
	return values, nil
}
func (s *memoryStore) SaveWorkspace(_ context.Context, id, base, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	value.BaseSHA, value.WorktreePath = base, path
	s.tasks[id] = value
	return nil
}
func (s *memoryStore) SaveTelegramMessage(_ context.Context, id string, messageID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	value.TelegramMessageID = messageID
	s.tasks[id] = value
	return nil
}
func (s *memoryStore) SaveProviderSession(_ context.Context, id string, session task.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	value.ProviderSessionID, value.ProviderThreadID = session.ProviderSessionID, session.ProviderThreadID
	s.tasks[id], s.sessions[session.ID] = value, session
	return nil
}
func (s *memoryStore) SaveDelivery(_ context.Context, id, commit, ref, deployment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	value.CommitSHA, value.PushRef, value.DeploymentURL = commit, ref, deployment
	s.tasks[id] = value
	return nil
}
func (s *memoryStore) SaveFailure(_ context.Context, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	value.FailureReason = reason
	s.tasks[id] = value
	return nil
}
func (*memoryStore) Close() error { return nil }

var _ = errors.Is
