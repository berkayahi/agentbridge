package localcontrol

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/deviceidentity"
	"github.com/berkayahi/agentbridge/internal/kernel"
	"github.com/berkayahi/agentbridge/internal/repository"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

type Service struct {
	commandMu sync.Mutex
	replayMu  sync.Mutex

	store      AuthorityStore
	identity   deviceidentity.Key
	runtimes   RuntimeCatalog
	controller CommandController
	executor   Executor
	verifier   Verifier
	committer  Committer
	clock      func() time.Time
	newID      func(string) string
}

func New(config Config) (*Service, error) {
	if config.Store == nil || config.Runtimes == nil {
		return nil, ErrInvalidRequest
	}
	if len(config.RemoteDevices) > 0 || config.RemoteDeviceFactory != nil {
		router := newDeviceRouter(config.Executor, config.Verifier, config.Committer, config.RemoteDevices, config.RemoteDeviceFactory)
		config.Executor, config.Verifier, config.Committer = router, router, router
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.NewID == nil {
		config.NewID = defaultID
	}
	return &Service{
		store: config.Store, identity: config.Identity, runtimes: config.Runtimes, controller: config.Controller,
		executor: config.Executor, verifier: config.Verifier, committer: config.Committer,
		clock: config.Clock, newID: config.NewID,
	}, nil
}

func (s *Service) CreateProject(ctx context.Context, request CreateProjectRequest) (ProjectResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	name := normalizeName(request.Name)
	payload := struct {
		Name string `json:"name"`
	}{Name: name}
	var cached ProjectResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "create_project", payload, &cached); done || err != nil {
		return cached, err
	}
	if name == "" {
		return ProjectResponse{}, fmt.Errorf("project name: %w", ErrInvalidRequest)
	}
	now := s.clock().UTC()
	project := Project{ID: s.newID("project"), Name: name, Revision: 1, CreatedAt: now, UpdatedAt: now}
	event := localEvent(s.newID("event"), "project", project.ID, "", 1, "project_created", map[string]any{"name": project.Name}, now)
	response := ProjectResponse{Project: project}
	if err := s.persistProjectCreation(ctx, request.IdempotencyKey, payload, project, event, response); err != nil {
		return ProjectResponse{}, err
	}
	return response, nil
}

func (s *Service) RegisterRepository(ctx context.Context, request RegisterRepositoryRequest) (RepositoryResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	remote := strings.TrimSpace(request.Remote)
	payload := struct {
		Remote string `json:"remote"`
	}{Remote: remote}
	var cached RepositoryResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "register_repository", payload, &cached); done || err != nil {
		return cached, err
	}
	if err := validateRemote(remote); err != nil {
		return RepositoryResponse{}, err
	}
	now := s.clock().UTC()
	value := Repository{ID: s.newID("repository"), Remote: remote, CreatedAt: now}
	event := localEvent(s.newID("event"), "repository", value.ID, "", 1, "repository_registered", map[string]any{"repository_id": value.ID}, now)
	response := RepositoryResponse{Repository: value}
	if err := s.persistRepositoryCreation(ctx, request.IdempotencyKey, payload, value, event, response); err != nil {
		return RepositoryResponse{}, err
	}
	return response, nil
}

func (s *Service) CreateBoard(ctx context.Context, request CreateBoardRequest) (BoardResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	name := normalizeName(request.Name)
	payload := struct {
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	}{ProjectID: request.ProjectID, Name: name}
	var cached BoardResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "create_board", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.ProjectID) || name == "" {
		return BoardResponse{}, ErrInvalidRequest
	}
	if _, err := s.store.GetProject(ctx, request.ProjectID); err != nil {
		return BoardResponse{}, err
	}
	now := s.clock().UTC()
	board := Board{ID: s.newID("board"), ProjectID: request.ProjectID, Name: name, Revision: 1, CreatedAt: now, UpdatedAt: now}
	event := localEvent(s.newID("event"), "board", board.ID, "", 1, "board_created", map[string]any{"project_id": board.ProjectID}, now)
	response := BoardResponse{Board: board}
	if err := s.persistBoardCreation(ctx, request.IdempotencyKey, payload, board, event, response); err != nil {
		return BoardResponse{}, err
	}
	return response, nil
}

func (s *Service) persistProjectCreation(ctx context.Context, key string, payload any, project Project, event Event, response ProjectResponse) error {
	if atomic, ok := s.store.(AtomicCreationAuthority); ok {
		record, err := s.creationIdempotencyRecord(key, "create_project", payload, response)
		if err != nil {
			return err
		}
		return atomic.CreateProjectAtomically(ctx, project, event, record)
	}
	if err := s.store.CreateProject(ctx, project); err != nil {
		return err
	}
	if _, err := s.store.AppendLocalEvent(ctx, event); err != nil {
		return err
	}
	return s.remember(ctx, key, "create_project", payload, response)
}

func (s *Service) persistRepositoryCreation(ctx context.Context, key string, payload any, repository Repository, event Event, response RepositoryResponse) error {
	if atomic, ok := s.store.(AtomicCreationAuthority); ok {
		record, err := s.creationIdempotencyRecord(key, "register_repository", payload, response)
		if err != nil {
			return err
		}
		return atomic.CreateRepositoryAtomically(ctx, repository, event, record)
	}
	if err := s.store.CreateRepository(ctx, repository); err != nil {
		return err
	}
	if _, err := s.store.AppendLocalEvent(ctx, event); err != nil {
		return err
	}
	return s.remember(ctx, key, "register_repository", payload, response)
}

func (s *Service) persistBoardCreation(ctx context.Context, key string, payload any, board Board, event Event, response BoardResponse) error {
	if atomic, ok := s.store.(AtomicCreationAuthority); ok {
		record, err := s.creationIdempotencyRecord(key, "create_board", payload, response)
		if err != nil {
			return err
		}
		return atomic.CreateBoardAtomically(ctx, board, event, record)
	}
	if err := s.store.CreateBoard(ctx, board); err != nil {
		return err
	}
	if _, err := s.store.AppendLocalEvent(ctx, event); err != nil {
		return err
	}
	return s.remember(ctx, key, "create_board", payload, response)
}

func (s *Service) creationIdempotencyRecord(key, operation string, payload, response any) (IdempotencyRecord, error) {
	encoded, err := json.Marshal(response)
	if err != nil {
		return IdempotencyRecord{}, err
	}
	return IdempotencyRecord{Key: key, Operation: operation, RequestHash: requestHash(operation, payload), ResponseBytes: encoded, CreatedAt: s.clock().UTC()}, nil
}

func (s *Service) CreateTask(ctx context.Context, request CreateTaskRequest) (TaskResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	targetDeviceID := strings.TrimSpace(request.TargetDeviceID)
	if targetDeviceID == "" {
		targetDeviceID = LocalDeviceID
	}
	payload := struct {
		ProjectID      string             `json:"project_id"`
		BoardID        string             `json:"board_id"`
		RepositoryID   string             `json:"repository_id"`
		TargetDeviceID string             `json:"target_device_id"`
		Provider       workmodel.Provider `json:"provider"`
		Title          string             `json:"title"`
		Prompt         string             `json:"prompt"`
	}{request.ProjectID, request.BoardID, request.RepositoryID, targetDeviceID, request.Provider, strings.TrimSpace(request.Title), strings.TrimSpace(request.Prompt)}
	var cached TaskResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "create_task", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.ProjectID) || !validID(request.BoardID) || !validID(request.RepositoryID) || !request.Provider.Valid() || payload.Prompt == "" {
		return TaskResponse{}, ErrInvalidRequest
	}
	if !validID(payload.TargetDeviceID) {
		return TaskResponse{}, ErrInvalidRequest
	}
	if _, err := s.runtimes.Get(string(request.Provider)); err != nil {
		return TaskResponse{}, fmt.Errorf("%w: %s", ErrUnknownProvider, request.Provider)
	}
	if _, err := s.store.GetProject(ctx, request.ProjectID); err != nil {
		return TaskResponse{}, err
	}
	board, err := s.store.GetBoard(ctx, request.BoardID)
	if err != nil {
		return TaskResponse{}, err
	}
	if board.ProjectID != request.ProjectID {
		return TaskResponse{}, fmt.Errorf("board project mismatch: %w", store.ErrConflict)
	}
	repository, err := s.store.GetRepository(ctx, request.RepositoryID)
	if err != nil {
		return TaskResponse{}, err
	}
	device, err := s.store.GetDevice(ctx, payload.TargetDeviceID)
	if err != nil {
		return TaskResponse{}, err
	}
	if err := requirePairedDevice(device); err != nil {
		return TaskResponse{}, err
	}
	now := s.clock().UTC()
	taskID := s.newID("task")
	title := payload.Title
	if title == "" {
		title = workmodel.Title(payload.Prompt, workmodel.DefaultTitleRunes)
	}
	task := workmodel.Task{ID: taskID, RepoProfileID: request.RepositoryID, Title: title, Prompt: payload.Prompt, State: workmodel.Queued, Provider: request.Provider, CreatedAt: now, UpdatedAt: now}
	initial := workmodel.Event{ID: s.newID("event"), TaskID: taskID, Type: workmodel.EventTaskCreated, Visibility: workmodel.VisibilityUser, Payload: json.RawMessage(`{"state":"queued"}`), CreatedAt: now}
	auditEvent := localEvent(s.newID("event"), "task", taskID, taskID, 1, "task_created", map[string]any{"project_id": request.ProjectID, "board_id": request.BoardID, "target_device_id": payload.TargetDeviceID}, now)
	// RuntimeStore keeps the fixed execution/session lineage and the initial
	// device assignment in the same transaction as this response. Building the
	// view from those canonical IDs lets a retry replay the exact response even
	// if the process dies immediately after commit.
	view := TaskView{
		ID: taskID, ProjectID: request.ProjectID, BoardID: request.BoardID, RepositoryID: request.RepositoryID,
		RepositoryRemote: repository.Remote,
		TargetDeviceID:   payload.TargetDeviceID, TargetEpoch: device.ConnectionEpoch,
		Title: title, Prompt: payload.Prompt, Provider: request.Provider, State: workmodel.Queued, Revision: 1,
		ExecutionID: taskID + "-execution", SessionID: taskID + "-session", RuntimeID: string(request.Provider),
		CreatedAt: now, UpdatedAt: now,
	}
	response := TaskResponse{Task: view}
	if atomic, ok := s.store.(AtomicCreationAuthority); ok {
		record, err := s.creationIdempotencyRecord(request.IdempotencyKey, "create_task", payload, response)
		if err != nil {
			return TaskResponse{}, err
		}
		if _, err := atomic.CreateTaskAtomically(ctx, AtomicTaskCreation{
			ProjectID: request.ProjectID, BoardID: request.BoardID, TargetDeviceID: payload.TargetDeviceID,
			Task: task, InitialEvent: initial, LocalEvent: auditEvent, Idempotency: record,
		}); err != nil {
			return TaskResponse{}, err
		}
		return response, nil
	}
	created, err := s.store.CreateTaskInContext(ctx, request.ProjectID, request.BoardID, payload.TargetDeviceID, task, initial, auditEvent)
	if err != nil {
		return TaskResponse{}, err
	}
	view, err = s.taskView(ctx, taskID)
	if err != nil {
		return TaskResponse{}, err
	}
	response = TaskResponse{Task: view}
	if err := s.remember(ctx, request.IdempotencyKey, "create_task", payload, response); err != nil {
		return TaskResponse{}, err
	}
	_ = created
	return response, nil
}

func (s *Service) UpdateTask(ctx context.Context, request UpdateTaskRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
		Title    string `json:"title"`
		Prompt   string `json:"prompt"`
	}{request.TaskID, request.Revision, normalizeName(request.Title), strings.TrimSpace(request.Prompt)}
	var cached ActionResponse
	if done, err := s.replay(ctx, request.IdempotencyKey, "update_task", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(payload.TaskID) || payload.Revision <= 0 || payload.Title == "" || payload.Prompt == "" {
		return ActionResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, payload.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if view.State != workmodel.Queued && view.State != workmodel.Paused {
		return ActionResponse{}, fmt.Errorf("update task in %s: %w", view.State, store.ErrConflict)
	}
	if err := checkRevision(view, payload.Revision); err != nil {
		return ActionResponse{}, err
	}
	now := s.clock().UTC()
	eventPayload := map[string]any{"title": payload.Title, "prompt": payload.Prompt}
	event := localEvent(s.newID("event"), "task", view.ID, view.ID, view.Revision+1, "task_updated", eventPayload, now)
	stored, err := s.store.UpdateTaskAtRevision(ctx, view.ID, view.Revision, payload.Title, payload.Prompt, event)
	if err != nil {
		return ActionResponse{}, err
	}
	next, err := s.taskView(ctx, view.ID)
	if err != nil {
		return ActionResponse{}, err
	}
	response := ActionResponse{Task: next, Event: stored}
	if err := s.remember(ctx, request.IdempotencyKey, "update_task", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func (s *Service) Observe(ctx context.Context, request ObserveRequest) (ObserveResponse, error) {
	if !validID(request.TaskID) {
		return ObserveResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ObserveResponse{}, err
	}
	view, err = s.refreshRemoteObservation(ctx, view)
	if err != nil {
		return ObserveResponse{}, err
	}
	limit := request.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	events, err := s.store.ListLocalEvents(ctx, request.TaskID, request.AfterCursor, limit)
	if err != nil {
		return ObserveResponse{}, err
	}
	response := ObserveResponse{Task: view, Events: events}
	if len(events) > 0 {
		response.NextCursor = events[len(events)-1].Cursor
	}
	return response, nil
}

func (s *Service) refreshRemoteObservation(ctx context.Context, view TaskView) (TaskView, error) {
	if view.TargetDeviceID == LocalDeviceID {
		return view, nil
	}
	observer, ok := s.executor.(DeviceObserver)
	if !ok {
		return view, nil
	}
	assignment, err := s.store.TaskDevice(ctx, view.ID)
	if err != nil {
		return view, err
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		if errors.Is(err, ErrDeviceRevoked) || errors.Is(err, ErrDeviceNotPaired) || errors.Is(err, ErrDeviceUnreachable) {
			return view, nil
		}
		return TaskView{}, err
	}
	if !available {
		return view, nil
	}
	observation, observeErr := observer.Observe(ctx, view, assignment.LastObservedCursor)
	switch {
	case observeErr == nil:
		if err := s.ingestDeviceObservation(ctx, view, observation); err != nil {
			return TaskView{}, err
		}
	case errors.Is(observeErr, ErrNotConfigured), isDeviceUnavailable(observeErr):
		// A device may be paired before its observation endpoint is available.
		// The controller's local event log remains readable; command replay will
		// retry the remote boundary.
	default:
		return TaskView{}, observeErr
	}
	return s.taskView(ctx, view.ID)
}

func (s *Service) ingestDeviceObservation(ctx context.Context, view TaskView, observation DeviceObservation) error {
	var previousCursor uint64
	seenEventIDs := make(map[string]struct{}, len(observation.Events))
	localEvents := make([]Event, 0, len(observation.Events))
	for _, remoteEvent := range observation.Events {
		if strings.TrimSpace(remoteEvent.ID) == "" || strings.TrimSpace(remoteEvent.Type) == "" || remoteEvent.TaskID != view.ID || remoteEvent.Cursor == 0 || remoteEvent.Cursor > observation.Cursor || remoteEvent.CreatedAt.IsZero() {
			return fmt.Errorf("device observation event: %w", ErrInvalidRequest)
		}
		if remoteEvent.Cursor <= previousCursor {
			return fmt.Errorf("device observation event cursor order: %w", ErrInvalidRequest)
		}
		if _, exists := seenEventIDs[remoteEvent.ID]; exists {
			return fmt.Errorf("device observation event %q repeated in one response: %w", remoteEvent.ID, ErrIdempotencyConflict)
		}
		seenEventIDs[remoteEvent.ID] = struct{}{}
		previousCursor = remoteEvent.Cursor
		eventPayload := append(json.RawMessage(nil), remoteEvent.Payload...)
		if len(eventPayload) == 0 {
			eventPayload = json.RawMessage(`{}`)
		}
		if !json.Valid(eventPayload) {
			return fmt.Errorf("device observation event payload: %w", ErrInvalidRequest)
		}
		payload := map[string]any{
			"device_id":     view.TargetDeviceID,
			"remote_cursor": remoteEvent.Cursor,
			"event_id":      remoteEvent.ID,
			"event_type":    remoteEvent.Type,
			"payload":       eventPayload,
		}
		created := remoteEvent.CreatedAt
		value := localEvent(
			"device-"+view.TargetDeviceID+"-"+view.ID+"-"+remoteEvent.ID,
			"task", view.ID, view.ID, view.Revision, "device_event", payload, created,
		)
		localEvents = append(localEvents, value)
	}
	approvals := make([]workmodel.Approval, 0, len(observation.Approvals))
	for _, remoteApproval := range observation.Approvals {
		if strings.TrimSpace(remoteApproval.ID) == "" || strings.TrimSpace(remoteApproval.Kind) == "" || remoteApproval.TaskID != view.ID || remoteApproval.Status != string(workmodel.ApprovalPending) || remoteApproval.RequestedAt.IsZero() {
			return fmt.Errorf("device observation approval: %w", ErrInvalidRequest)
		}
		requestPayload := append(json.RawMessage(nil), remoteApproval.RequestPayload...)
		if len(requestPayload) == 0 {
			requestPayload = json.RawMessage(`{}`)
		}
		if !json.Valid(requestPayload) {
			return fmt.Errorf("device observation approval payload: %w", ErrInvalidRequest)
		}
		requestedAt := remoteApproval.RequestedAt
		approvals = append(approvals, workmodel.Approval{
			ID: remoteApproval.ID, TaskID: remoteApproval.TaskID, Kind: remoteApproval.Kind,
			Status: workmodel.ApprovalPending, RequestPayload: requestPayload,
			RequestedAt: requestedAt, ExpiresAt: remoteApproval.ExpiresAt,
		})
	}
	return s.store.ApplyDeviceObservation(ctx, view.ID, view.TargetDeviceID, view.TargetEpoch, view.Revision, observation.Cursor, localEvents, approvals)
}

// PendingApprovals returns the task-scoped approval records that a local
// client may resolve. The provider request payload is already redacted at its
// durable sink; exposing it here lets a client display the real approval ID
// without inventing a presentation-side identifier.
func (s *Service) PendingApprovals(ctx context.Context, taskID string) (ApprovalsResponse, error) {
	if !validID(taskID) {
		return ApprovalsResponse{}, ErrInvalidRequest
	}
	if _, err := s.store.Task(ctx, taskID); err != nil {
		return ApprovalsResponse{}, err
	}
	values, err := s.store.PendingApprovals(ctx)
	if err != nil {
		return ApprovalsResponse{}, err
	}
	now := s.clock().UTC()
	response := ApprovalsResponse{Approvals: make([]ApprovalView, 0)}
	for _, value := range values {
		if value.TaskID != taskID || (value.ExpiresAt != nil && !value.ExpiresAt.After(now)) {
			continue
		}
		response.Approvals = append(response.Approvals, ApprovalView{
			ID: value.ID, TaskID: value.TaskID, Kind: value.Kind, Status: string(value.Status),
			RequestPayload: append(json.RawMessage(nil), value.RequestPayload...),
			RequestedAt:    value.RequestedAt, ExpiresAt: value.ExpiresAt,
		})
	}
	return response, nil
}

func (s *Service) Start(ctx context.Context, request StartRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID         string `json:"task_id"`
		Revision       int64  `json:"revision"`
		Input          string `json:"input"`
		Model          string `json:"model"`
		PolicySnapshot []byte `json:"policy_snapshot"`
	}{request.TaskID, request.Revision, strings.TrimSpace(request.Input), request.Model, request.PolicySnapshot}
	var cached ActionResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "start", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 {
		return ActionResponse{}, ErrInvalidRequest
	}
	if s.executor == nil {
		return ActionResponse{}, ErrNotConfigured
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return ActionResponse{}, err
	}
	preparedReplay := isDeviceReplay(ctx) && view.TargetDeviceID != LocalDeviceID && view.State == workmodel.Preparing
	runningReplay := isDeviceReplay(ctx) && view.TargetDeviceID != LocalDeviceID && view.State == workmodel.Running
	if view.State != workmodel.Queued && view.State != workmodel.Paused && !preparedReplay && !runningReplay {
		return ActionResponse{}, fmt.Errorf("start task in %s: %w", view.State, store.ErrConflict)
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return ActionResponse{}, err
	}
	if !available {
		event, queueErr := s.queueUnavailable(ctx, view, "start", request.IdempotencyKey, payload, request, ErrDeviceUnreachable)
		if queueErr != nil {
			return ActionResponse{}, queueErr
		}
		response := ActionResponse{Task: view, Event: event, Queued: true}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "start", payload, response); err != nil {
			return ActionResponse{}, err
		}
		return response, nil
	}
	info, err := s.store.ExecutionInfo(ctx, view.ID)
	if err != nil {
		return ActionResponse{}, err
	}
	input := payload.Input
	if input == "" {
		input = view.Prompt
	}
	policy := append([]byte(nil), payload.PolicySnapshot...)
	if len(policy) == 0 {
		policy = []byte(`{}`)
	}
	// The standalone kernel is the local runtime controller. A paired target
	// receives the typed command through the device runtime below; invoking the
	// kernel here would start a second local execution for the same task.
	if view.TargetDeviceID == LocalDeviceID && s.controller != nil {
		if err := s.controller.Start(ctx, kernel.StartExecution{
			CommandID: s.newID("command"), ExecutionID: info.ExecutionID, TaskID: view.ID, SessionID: info.SessionID,
			RepositoryID: info.RepositoryID, RuntimeID: info.RuntimeID, Model: payload.Model, PolicySnapshot: policy,
			FencingEpoch: info.FencingEpoch, Input: kernel.Input{Text: input},
		}); err != nil {
			return ActionResponse{}, err
		}
	}
	startView := view
	if view.State == workmodel.Paused {
		startView, _, err = s.transition(ctx, view, workmodel.Queued, "start_requeued", nil)
		if err != nil {
			return ActionResponse{}, err
		}
	}
	preparing := startView
	if startView.State != workmodel.Preparing {
		preparing, _, err = s.transition(ctx, startView, workmodel.Preparing, "start_accepted", map[string]any{"execution_id": info.ExecutionID})
		if err != nil {
			return ActionResponse{}, err
		}
	}
	command, remote, err := s.enqueueDeviceCommand(ctx, preparing, "start", request.IdempotencyKey, payload, request, true)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := s.executor.Start(ctx, preparing, request); err != nil {
		current, refreshErr := s.taskView(ctx, preparing.ID)
		if refreshErr != nil {
			return ActionResponse{}, errors.Join(err, refreshErr)
		}
		if remote && isDeviceUnavailable(err) {
			paused, _, transitionErr := s.transition(ctx, current, workmodel.Paused, "start_deferred", map[string]any{"message": safeError(err)})
			if transitionErr != nil {
				return ActionResponse{}, errors.Join(err, transitionErr)
			}
			queuedEvent, queueErr := s.deferDeviceCommand(ctx, command, paused, err)
			if queueErr != nil {
				return ActionResponse{}, errors.Join(err, queueErr)
			}
			response := ActionResponse{Task: paused, Event: queuedEvent, Queued: true}
			if rememberErr := s.rememberAction(ctx, request.IdempotencyKey, "start", payload, response); rememberErr != nil {
				return ActionResponse{}, errors.Join(err, rememberErr)
			}
			return response, nil
		}
		if remote {
			_ = s.failDeviceCommand(ctx, command, err)
		}
		failed, _, transitionErr := s.transition(ctx, current, workmodel.Failed, "start_failed", map[string]any{"message": safeError(err)})
		if transitionErr != nil {
			return ActionResponse{}, errors.Join(err, transitionErr)
		}
		_ = failed
		return ActionResponse{}, err
	}
	current, err := s.taskView(ctx, preparing.ID)
	if err != nil {
		return ActionResponse{}, err
	}
	running := current
	var startedEvent Event
	if runningReplay {
		startedEvent, err = s.latestLocalEvent(ctx, current.ID, "started")
		if errors.Is(err, store.ErrNotFound) {
			startedEvent, err = s.store.AppendLocalEvent(ctx, localEvent(s.newID("event"), "task", current.ID, current.ID, current.Revision, "started", map[string]any{"execution_id": info.ExecutionID}, s.clock().UTC()))
		}
		if err != nil {
			return ActionResponse{}, err
		}
	} else {
		running, startedEvent, err = s.transition(ctx, current, workmodel.Running, "started", map[string]any{"execution_id": info.ExecutionID})
		if err != nil {
			return ActionResponse{}, err
		}
	}
	if err := s.completeDeviceCommand(ctx, command); err != nil {
		return ActionResponse{}, err
	}
	response := ActionResponse{Task: running, Event: startedEvent}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "start", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func (s *Service) Approve(ctx context.Context, request ApproveRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID     string `json:"task_id"`
		ApprovalID string `json:"approval_id"`
		Revision   int64  `json:"revision"`
		UserID     string `json:"user_id"`
		Allow      bool   `json:"allow"`
	}{request.TaskID, request.ApprovalID, request.Revision, strings.TrimSpace(request.UserID), request.Allow}
	var cached ActionResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "approve", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || !validID(request.ApprovalID) || request.Revision <= 0 || payload.UserID == "" {
		return ActionResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return ActionResponse{}, err
	}
	approval, err := s.store.GetApproval(ctx, request.ApprovalID)
	if errors.Is(err, store.ErrNotFound) {
		return ActionResponse{}, ErrApprovalNotPending
	}
	if err != nil {
		return ActionResponse{}, err
	}
	if approval.TaskID != request.TaskID {
		return ActionResponse{}, ErrApprovalNotPending
	}
	status := workmodel.ApprovalRejected
	if request.Allow {
		status = workmodel.ApprovalApproved
	}
	approvalWasPending := approval.Status == workmodel.ApprovalPending
	if !approvalWasPending && (!isDeviceReplay(ctx) || approval.Status != status) {
		return ActionResponse{}, ErrApprovalNotPending
	}
	if approvalWasPending && approval.ExpiresAt != nil && !approval.ExpiresAt.After(s.clock().UTC()) {
		return ActionResponse{}, ErrApprovalNotPending
	}
	if !approvalWasPending {
		if event, ok, err := s.recordedApproval(ctx, view, request.ApprovalID, request.Allow); err != nil {
			return ActionResponse{}, err
		} else if ok {
			if err := s.completeExistingDeviceCommand(ctx, request.IdempotencyKey, view, "approve"); err != nil {
				return ActionResponse{}, err
			}
			response := ActionResponse{Task: view, Event: event}
			if err := s.rememberAction(ctx, request.IdempotencyKey, "approve", payload, response); err != nil {
				return ActionResponse{}, err
			}
			return response, nil
		}
	}
	if s.executor == nil {
		return ActionResponse{}, ErrNotConfigured
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return ActionResponse{}, err
	}
	if !available {
		event, queueErr := s.queueUnavailable(ctx, view, "approve", request.IdempotencyKey, payload, request, ErrDeviceUnreachable)
		if queueErr != nil {
			return ActionResponse{}, queueErr
		}
		response := ActionResponse{Task: view, Event: event, Queued: true}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "approve", payload, response); err != nil {
			return ActionResponse{}, err
		}
		return response, nil
	}
	command, remote, err := s.enqueueDeviceCommand(ctx, view, "approve", request.IdempotencyKey, payload, request, true)
	if err != nil {
		return ActionResponse{}, err
	}
	resolved := s.clock().UTC()
	decision, _ := json.Marshal(map[string]any{"allow": request.Allow, "user_id": payload.UserID})
	updated := approval
	updated.Status, updated.DecisionPayload, updated.ResolvedAt = status, decision, &resolved
	if approvalWasPending {
		if err := s.store.UpsertApproval(ctx, updated); err != nil {
			if remote {
				_ = s.failDeviceCommand(ctx, command, err)
			}
			return ActionResponse{}, err
		}
	}
	if err := s.executor.Approve(ctx, view, request.ApprovalID, payload.UserID, request.Allow); err != nil {
		if approvalWasPending {
			updated.Status, updated.DecisionPayload, updated.ResolvedAt = workmodel.ApprovalPending, nil, nil
		}
		if remote && isDeviceUnavailable(err) {
			current, refreshErr := s.taskView(ctx, request.TaskID)
			if refreshErr != nil {
				if approvalWasPending {
					return ActionResponse{}, errors.Join(err, refreshErr, s.store.UpsertApproval(ctx, updated))
				}
				return ActionResponse{}, errors.Join(err, refreshErr)
			}
			queuedEvent, queueErr := s.deferDeviceCommand(ctx, command, current, err)
			response := ActionResponse{Task: current, Event: queuedEvent, Queued: true}
			var approvalErr error
			if approvalWasPending {
				approvalErr = s.store.UpsertApproval(ctx, updated)
			}
			if joinedErr := errors.Join(queueErr, approvalErr); joinedErr != nil {
				return ActionResponse{}, joinedErr
			}
			if err := s.rememberAction(ctx, request.IdempotencyKey, "approve", payload, response); err != nil {
				return ActionResponse{}, err
			}
			return response, nil
		}
		if remote {
			_ = s.failDeviceCommand(ctx, command, err)
		}
		var approvalErr error
		if approvalWasPending {
			approvalErr = s.store.UpsertApproval(ctx, updated)
		}
		return ActionResponse{}, errors.Join(err, approvalErr)
	}
	view, err = s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	to := view.State
	if view.State == workmodel.AwaitingApproval {
		if request.Allow {
			to = workmodel.Running
		} else {
			to = workmodel.Failed
		}
	}
	var event Event
	var next TaskView
	if to != view.State {
		next, event, err = s.transition(ctx, view, to, "approval_resolved", map[string]any{"approval_id": request.ApprovalID, "allow": request.Allow})
	} else {
		next = view
		event, err = s.store.AppendLocalEvent(ctx, localEvent(s.newID("event"), "task", view.ID, view.ID, view.Revision, "approval_resolved", map[string]any{"approval_id": request.ApprovalID, "allow": request.Allow}, resolved))
	}
	if err != nil {
		return ActionResponse{}, err
	}
	response := ActionResponse{Task: next, Event: event}
	if err := s.completeDeviceCommand(ctx, command); err != nil {
		return ActionResponse{}, err
	}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "approve", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func (s *Service) Resume(ctx context.Context, request ResumeRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
		Input    string `json:"input"`
	}{request.TaskID, request.Revision, strings.TrimSpace(request.Input)}
	var cached ActionResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "resume", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 {
		return ActionResponse{}, ErrInvalidRequest
	}
	if s.executor == nil {
		return ActionResponse{}, ErrNotConfigured
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return ActionResponse{}, err
	}
	preparedReplay := isDeviceReplay(ctx) && view.TargetDeviceID != LocalDeviceID && view.State == workmodel.Preparing
	runningReplay := isDeviceReplay(ctx) && view.TargetDeviceID != LocalDeviceID && view.State == workmodel.Running
	if view.State != workmodel.Paused && view.State != workmodel.Failed && !preparedReplay && !runningReplay {
		return ActionResponse{}, fmt.Errorf("resume task in %s: %w", view.State, store.ErrConflict)
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return ActionResponse{}, err
	}
	if !available {
		event, queueErr := s.queueUnavailable(ctx, view, "resume", request.IdempotencyKey, payload, request, ErrDeviceUnreachable)
		if queueErr != nil {
			return ActionResponse{}, queueErr
		}
		response := ActionResponse{Task: view, Event: event, Queued: true}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "resume", payload, response); err != nil {
			return ActionResponse{}, err
		}
		return response, nil
	}
	preparing := view
	if !preparedReplay {
		queued, _, transitionErr := s.transition(ctx, view, workmodel.Queued, "resume_queued", nil)
		if transitionErr != nil {
			return ActionResponse{}, transitionErr
		}
		preparing, _, transitionErr = s.transition(ctx, queued, workmodel.Preparing, "resume_accepted", nil)
		if transitionErr != nil {
			return ActionResponse{}, transitionErr
		}
	}
	command, remote, err := s.enqueueDeviceCommand(ctx, preparing, "resume", request.IdempotencyKey, payload, request, true)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := s.executor.Resume(ctx, preparing, request); err != nil {
		current, refreshErr := s.taskView(ctx, preparing.ID)
		if refreshErr != nil {
			return ActionResponse{}, errors.Join(err, refreshErr)
		}
		if remote && isDeviceUnavailable(err) {
			paused, _, transitionErr := s.transition(ctx, current, workmodel.Paused, "resume_deferred", map[string]any{"message": safeError(err)})
			if transitionErr != nil {
				return ActionResponse{}, errors.Join(err, transitionErr)
			}
			queuedEvent, queueErr := s.deferDeviceCommand(ctx, command, paused, err)
			if queueErr != nil {
				return ActionResponse{}, errors.Join(err, queueErr)
			}
			response := ActionResponse{Task: paused, Event: queuedEvent, Queued: true}
			if rememberErr := s.rememberAction(ctx, request.IdempotencyKey, "resume", payload, response); rememberErr != nil {
				return ActionResponse{}, errors.Join(err, rememberErr)
			}
			return response, nil
		}
		if remote {
			_ = s.failDeviceCommand(ctx, command, err)
		}
		_, _, transitionErr := s.transition(ctx, current, workmodel.Failed, "resume_failed", map[string]any{"message": safeError(err)})
		return ActionResponse{}, errors.Join(err, transitionErr)
	}
	current, err := s.taskView(ctx, preparing.ID)
	if err != nil {
		return ActionResponse{}, err
	}
	running := current
	var event Event
	if runningReplay {
		event, err = s.latestLocalEvent(ctx, current.ID, "resumed")
		if errors.Is(err, store.ErrNotFound) {
			event, err = s.store.AppendLocalEvent(ctx, localEvent(s.newID("event"), "task", current.ID, current.ID, current.Revision, "resumed", nil, s.clock().UTC()))
		}
		if err != nil {
			return ActionResponse{}, err
		}
	} else {
		running, event, err = s.transition(ctx, current, workmodel.Running, "resumed", nil)
		if err != nil {
			return ActionResponse{}, err
		}
	}
	if err := s.completeDeviceCommand(ctx, command); err != nil {
		return ActionResponse{}, err
	}
	response := ActionResponse{Task: running, Event: event}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "resume", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func (s *Service) Cancel(ctx context.Context, request CancelRequest) (ActionResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
	}{request.TaskID, request.Revision}
	var cached ActionResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "cancel", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 {
		return ActionResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return ActionResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return ActionResponse{}, err
	}
	if view.State == workmodel.Canceled {
		return ActionResponse{}, fmt.Errorf("task already canceled: %w", store.ErrConflict)
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return ActionResponse{}, err
	}
	if !available {
		event, queueErr := s.queueUnavailable(ctx, view, "cancel", request.IdempotencyKey, payload, request, ErrDeviceUnreachable)
		if queueErr != nil {
			return ActionResponse{}, queueErr
		}
		response := ActionResponse{Task: view, Event: event, Queued: true}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "cancel", payload, response); err != nil {
			return ActionResponse{}, err
		}
		return response, nil
	}
	info, err := s.store.ExecutionInfo(ctx, view.ID)
	if err != nil {
		return ActionResponse{}, err
	}
	command, remote, err := s.enqueueDeviceCommand(ctx, view, "cancel", request.IdempotencyKey, payload, request, true)
	if err != nil {
		return ActionResponse{}, err
	}
	// Cancellation follows the same authority boundary as start: the local
	// kernel may interrupt only a local runtime, while a paired target is
	// interrupted by its authenticated device command.
	if view.TargetDeviceID == LocalDeviceID && s.controller != nil {
		if err := s.controller.Cancel(ctx, kernel.CancelExecution{CommandID: s.newID("command"), ExecutionID: info.ExecutionID, TaskID: view.ID, RuntimeID: info.RuntimeID}); err != nil {
			if remote {
				_ = s.failDeviceCommand(ctx, command, err)
			}
			return ActionResponse{}, err
		}
	}
	if s.executor != nil {
		// The controller's durable cancellation intent is written before this
		// provider interruption. A crash between them is reconciled as fenced.
	}
	next, event, err := s.transition(ctx, view, workmodel.Canceled, "canceled", map[string]any{"execution_id": info.ExecutionID})
	if err != nil {
		return ActionResponse{}, err
	}
	if s.executor != nil {
		if err := s.executor.Cancel(ctx, next); err != nil {
			if remote && isDeviceUnavailable(err) {
				queuedEvent, queueErr := s.deferDeviceCommand(ctx, command, next, err)
				response := ActionResponse{Task: next, Event: queuedEvent, Queued: true}
				if queueErr != nil {
					return ActionResponse{}, queueErr
				}
				if err := s.rememberAction(ctx, request.IdempotencyKey, "cancel", payload, response); err != nil {
					return ActionResponse{}, err
				}
				return response, nil
			}
			if remote {
				_ = s.failDeviceCommand(ctx, command, err)
			}
			return ActionResponse{}, err
		}
	}
	response := ActionResponse{Task: next, Event: event}
	if err := s.completeDeviceCommand(ctx, command); err != nil {
		return ActionResponse{}, err
	}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "cancel", payload, response); err != nil {
		return ActionResponse{}, err
	}
	return response, nil
}

func (s *Service) Verify(ctx context.Context, request VerifyRequest) (VerifyResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
	}{request.TaskID, request.Revision}
	var cached VerifyResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "verify", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 {
		return VerifyResponse{}, ErrInvalidRequest
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return VerifyResponse{}, err
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return VerifyResponse{}, err
	}
	if view.State != workmodel.Running && view.State != workmodel.Verifying {
		return VerifyResponse{}, ErrVerificationRequired
	}
	if recorded, ok, err := s.recordedVerification(ctx, view); err != nil {
		return VerifyResponse{}, err
	} else if ok {
		if err := s.completeExistingDeviceCommand(ctx, request.IdempotencyKey, view, "verify"); err != nil {
			return VerifyResponse{}, err
		}
		response := VerifyResponse{Task: view, Receipt: recorded.Receipt, Event: recorded.Event}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "verify", payload, response); err != nil {
			return VerifyResponse{}, err
		}
		return response, nil
	}
	if s.verifier == nil {
		return VerifyResponse{}, ErrNotConfigured
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return VerifyResponse{}, err
	}
	if !available {
		event, queueErr := s.queueUnavailable(ctx, view, "verify", request.IdempotencyKey, payload, request, ErrDeviceUnreachable)
		if queueErr != nil {
			return VerifyResponse{}, queueErr
		}
		response := VerifyResponse{Task: view, Event: event, Queued: true}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "verify", payload, response); err != nil {
			return VerifyResponse{}, err
		}
		return response, nil
	}
	if view.State == workmodel.Running {
		view, _, err = s.transition(ctx, view, workmodel.Verifying, "verification_started", nil)
		if err != nil {
			return VerifyResponse{}, err
		}
	}
	command, remote, err := s.enqueueDeviceCommand(ctx, view, "verify", request.IdempotencyKey, payload, request, true)
	if err != nil {
		return VerifyResponse{}, err
	}
	receipt, verifyErr := s.verifier.Verify(ctx, view)
	if receipt.ID == "" {
		receipt.ID = s.newID("verification")
	}
	if receipt.ObservedAt.IsZero() {
		receipt.ObservedAt = s.clock().UTC()
	}
	if verifyErr != nil && remote && isDeviceUnavailable(verifyErr) {
		queuedEvent, queueErr := s.deferDeviceCommand(ctx, command, view, verifyErr)
		response := VerifyResponse{Task: view, Event: queuedEvent, Queued: true}
		if queueErr != nil {
			return VerifyResponse{}, queueErr
		}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "verify", payload, response); err != nil {
			return VerifyResponse{}, err
		}
		return response, nil
	}
	if remote && verifyErr != nil {
		_ = s.failDeviceCommand(ctx, command, verifyErr)
	}
	if verifyErr != nil || !receipt.Passed {
		_, _, transitionErr := s.transition(ctx, view, workmodel.Failed, "verification_failed", map[string]any{"receipt_id": receipt.ID, "summary": receipt.Summary})
		return VerifyResponse{}, errors.Join(verifyErr, transitionErr, ErrVerificationRequired)
	}
	event, err := s.store.AppendLocalEvent(ctx, localEvent(s.newID("event"), "task", view.ID, view.ID, view.Revision, "verification_passed", receipt, receipt.ObservedAt))
	if err != nil {
		return VerifyResponse{}, err
	}
	if err := s.completeDeviceCommand(ctx, command); err != nil {
		return VerifyResponse{}, err
	}
	response := VerifyResponse{Task: view, Receipt: receipt, Event: event}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "verify", payload, response); err != nil {
		return VerifyResponse{}, err
	}
	return response, nil
}

func (s *Service) Commit(ctx context.Context, request CommitRequest) (CommitResponse, error) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()

	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
	}{request.TaskID, request.Revision}
	var cached CommitResponse
	if done, err := s.replayAction(ctx, request.IdempotencyKey, "commit", payload, &cached); done || err != nil {
		return cached, err
	}
	if !validID(request.TaskID) || request.Revision <= 0 {
		return CommitResponse{}, ErrInvalidRequest
	}
	if s.committer == nil {
		return CommitResponse{}, ErrNotConfigured
	}
	view, err := s.taskView(ctx, request.TaskID)
	if err != nil {
		return CommitResponse{}, err
	}
	recorded, checkpointErr := s.store.LoadCheckpoint(ctx, view.ID)
	if checkpointErr != nil && !errors.Is(checkpointErr, store.ErrNotFound) {
		return CommitResponse{}, checkpointErr
	}
	if checkpointErr == nil && request.Revision <= view.Revision {
		if view.State == workmodel.Completed && view.CommitSHA == recorded.CommitSHA {
			response, err := s.recordedCommitResponse(ctx, request, view, recorded)
			if err != nil {
				return CommitResponse{}, err
			}
			return response, nil
		}
		if view.State == workmodel.Committing || view.State == workmodel.Pushing {
			response, err := s.finishRecordedCommit(ctx, request, view, recorded)
			if err != nil {
				return CommitResponse{}, err
			}
			return response, nil
		}
	}
	if err := checkRevision(view, request.Revision); err != nil {
		return CommitResponse{}, err
	}
	if view.State != workmodel.Verifying && view.State != workmodel.Committing {
		return CommitResponse{}, ErrCommitRequired
	}
	available, err := s.targetAvailability(ctx, view)
	if err != nil {
		return CommitResponse{}, err
	}
	if !available {
		event, queueErr := s.queueUnavailable(ctx, view, "commit", request.IdempotencyKey, payload, request, ErrDeviceUnreachable)
		if queueErr != nil {
			return CommitResponse{}, queueErr
		}
		response := CommitResponse{Task: view, Event: event, Queued: true}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "commit", payload, response); err != nil {
			return CommitResponse{}, err
		}
		return response, nil
	}
	committing := view
	if view.State == workmodel.Verifying {
		committing, _, err = s.transition(ctx, view, workmodel.Committing, "commit_started", nil)
		if err != nil {
			return CommitResponse{}, err
		}
	}
	command, remote, err := s.enqueueDeviceCommand(ctx, committing, "commit", request.IdempotencyKey, payload, request, true)
	if err != nil {
		return CommitResponse{}, err
	}
	receipt, commitErr := s.committer.Commit(ctx, committing)
	if receipt.ID == "" {
		receipt.ID = s.newID("commit")
	}
	if receipt.ObservedAt.IsZero() {
		receipt.ObservedAt = s.clock().UTC()
	}
	if commitErr != nil && remote && isDeviceUnavailable(commitErr) {
		queuedEvent, queueErr := s.deferDeviceCommand(ctx, command, committing, commitErr)
		response := CommitResponse{Task: committing, Event: queuedEvent, Queued: true}
		if queueErr != nil {
			return CommitResponse{}, queueErr
		}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "commit", payload, response); err != nil {
			return CommitResponse{}, err
		}
		return response, nil
	}
	if remote && commitErr != nil {
		_ = s.failDeviceCommand(ctx, command, commitErr)
	}
	if commitErr != nil {
		_, _, transitionErr := s.transition(ctx, committing, workmodel.Failed, "commit_failed", map[string]any{"message": safeError(commitErr)})
		return CommitResponse{}, errors.Join(commitErr, transitionErr)
	}
	if _, err := repository.NewCheckpoint(repository.CheckpointInput{ID: receipt.ID, RepositoryID: committing.RepositoryID, CommitSHA: receipt.CommitSHA, RemoteRef: receipt.RemoteRef, CreatedAt: receipt.ObservedAt}); err != nil {
		return CommitResponse{}, err
	}
	if err := s.store.RecordCheckpoint(ctx, committing.ID, receipt); err != nil {
		return CommitResponse{}, err
	}
	if err := s.store.SaveDelivery(ctx, committing.ID, receipt.CommitSHA, receipt.RemoteRef, ""); err != nil {
		return CommitResponse{}, err
	}
	current, err := s.taskView(ctx, committing.ID)
	if err != nil {
		return CommitResponse{}, err
	}
	if err := s.completeDeviceCommand(ctx, command); err != nil {
		return CommitResponse{}, err
	}
	if current.State != workmodel.Committing {
		return CommitResponse{}, fmt.Errorf("commit state changed: %w", store.ErrConflict)
	}
	pushing, _, err := s.transition(ctx, current, workmodel.Pushing, "commit_recorded", map[string]any{"commit_sha": receipt.CommitSHA, "remote_ref": receipt.RemoteRef})
	if err != nil {
		return CommitResponse{}, err
	}
	completed, event, err := s.transition(ctx, pushing, workmodel.Completed, "commit_completed", map[string]any{"commit_sha": receipt.CommitSHA, "remote_ref": receipt.RemoteRef})
	if err != nil {
		return CommitResponse{}, err
	}
	response := CommitResponse{Task: completed, Receipt: receipt, Event: event}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "commit", payload, response); err != nil {
		return CommitResponse{}, err
	}
	return response, nil
}

func (s *Service) finishRecordedCommit(ctx context.Context, request CommitRequest, view TaskView, receipt CommitReceipt) (CommitResponse, error) {
	if view.CommitSHA != receipt.CommitSHA {
		if err := s.store.SaveDelivery(ctx, view.ID, receipt.CommitSHA, receipt.RemoteRef, ""); err != nil {
			return CommitResponse{}, err
		}
		var err error
		view, err = s.taskView(ctx, view.ID)
		if err != nil {
			return CommitResponse{}, err
		}
	}
	if view.State == workmodel.Committing {
		var err error
		view, _, err = s.transition(ctx, view, workmodel.Pushing, "commit_recorded", map[string]any{"commit_sha": receipt.CommitSHA, "remote_ref": receipt.RemoteRef})
		if err != nil {
			return CommitResponse{}, err
		}
	}
	if view.State == workmodel.Pushing {
		var event Event
		var err error
		view, event, err = s.transition(ctx, view, workmodel.Completed, "commit_completed", map[string]any{"commit_sha": receipt.CommitSHA, "remote_ref": receipt.RemoteRef})
		if err != nil {
			return CommitResponse{}, err
		}
		if err := s.completePendingDeviceCommands(ctx, view, "commit"); err != nil {
			return CommitResponse{}, err
		}
		response := CommitResponse{Task: view, Receipt: receipt, Event: event}
		if err := s.rememberAction(ctx, request.IdempotencyKey, "commit", struct {
			TaskID   string `json:"task_id"`
			Revision int64  `json:"revision"`
		}{request.TaskID, request.Revision}, response); err != nil {
			return CommitResponse{}, err
		}
		return response, nil
	}
	return s.recordedCommitResponse(ctx, request, view, receipt)
}

func (s *Service) recordedCommitResponse(ctx context.Context, request CommitRequest, view TaskView, receipt CommitReceipt) (CommitResponse, error) {
	events, err := s.store.ListLocalEvents(ctx, view.ID, 0, 200)
	if err != nil {
		return CommitResponse{}, err
	}
	var event Event
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == "commit_completed" {
			event = events[index]
			break
		}
	}
	if event.ID == "" {
		return CommitResponse{}, fmt.Errorf("completed commit event is missing: %w", store.ErrConflict)
	}
	if err := s.completePendingDeviceCommands(ctx, view, "commit"); err != nil {
		return CommitResponse{}, err
	}
	response := CommitResponse{Task: view, Receipt: receipt, Event: event}
	payload := struct {
		TaskID   string `json:"task_id"`
		Revision int64  `json:"revision"`
	}{request.TaskID, request.Revision}
	if err := s.rememberAction(ctx, request.IdempotencyKey, "commit", payload, response); err != nil {
		return CommitResponse{}, err
	}
	return response, nil
}

type recordedVerification struct {
	Receipt VerificationReceipt
	Event   Event
}

func (s *Service) recordedApproval(ctx context.Context, view TaskView, approvalID string, allow bool) (Event, bool, error) {
	events, err := s.store.ListLocalEvents(ctx, view.ID, 0, 200)
	if err != nil {
		return Event{}, false, err
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != "approval_resolved" || event.Revision != view.Revision {
			continue
		}
		var payload struct {
			ApprovalID string `json:"approval_id"`
			Allow      bool   `json:"allow"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ApprovalID == "" {
			return Event{}, false, fmt.Errorf("recorded approval evidence: %w", store.ErrConflict)
		}
		if payload.ApprovalID == approvalID && payload.Allow == allow {
			return event, true, nil
		}
	}
	return Event{}, false, nil
}

// recordedVerification returns only evidence for the current task revision.
// A prior verification event from an earlier retry/run must not make a new
// verification appear complete after the task has been resumed.
func (s *Service) recordedVerification(ctx context.Context, view TaskView) (recordedVerification, bool, error) {
	events, err := s.store.ListLocalEvents(ctx, view.ID, 0, 200)
	if err != nil {
		return recordedVerification{}, false, err
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != "verification_passed" || event.Revision != view.Revision {
			continue
		}
		var receipt VerificationReceipt
		if err := json.Unmarshal(event.Payload, &receipt); err != nil || receipt.ID == "" || !receipt.Passed || receipt.ObservedAt.IsZero() {
			return recordedVerification{}, false, fmt.Errorf("recorded verification evidence: %w", store.ErrConflict)
		}
		return recordedVerification{Receipt: receipt, Event: event}, true, nil
	}
	return recordedVerification{}, false, nil
}

// completeExistingDeviceCommand closes a remote command only when the
// request key names a matching task/device operation. A new idempotent Verify
// request may legitimately have no command row because the original command
// was already completed before its response was persisted.
func (s *Service) completeExistingDeviceCommand(ctx context.Context, id string, view TaskView, operation string) error {
	if view.TargetDeviceID == LocalDeviceID {
		return nil
	}
	record, err := s.store.GetDeviceCommand(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.TaskID != view.ID || record.DeviceID != view.TargetDeviceID || record.AssignmentEpoch != view.TargetEpoch {
		return fmt.Errorf("complete recorded device command: %w", ErrDeviceFence)
	}
	if record.Operation != operation {
		return fmt.Errorf("complete recorded device command: %w", ErrIdempotencyConflict)
	}
	return s.completeDeviceCommand(ctx, record)
}

func (s *Service) taskView(ctx context.Context, id string) (TaskView, error) {
	task, err := s.store.Task(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	if task.ControllerOwner != "" && task.ControllerOwner != workmodel.TaskControllerLocal {
		return TaskView{}, ErrTaskOwnedByAnotherController
	}
	projectID, boardID, err := s.store.TaskContext(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	info, err := s.store.ExecutionInfo(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	assignment, err := s.store.TaskDevice(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	repository, err := s.store.GetRepository(ctx, task.RepoProfileID)
	if err != nil {
		return TaskView{}, err
	}
	return TaskView{ID: task.ID, ProjectID: projectID, BoardID: boardID, RepositoryID: task.RepoProfileID, RepositoryRemote: repository.Remote, TargetDeviceID: assignment.DeviceID, TargetEpoch: assignment.AssignmentEpoch, Title: task.Title, Prompt: task.Prompt, Provider: task.Provider, State: task.State, Revision: task.Revision, ExecutionID: info.ExecutionID, SessionID: info.SessionID, RuntimeID: info.RuntimeID, CommitSHA: task.CommitSHA, PushRef: task.PushRef, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt}, nil
}

func (s *Service) ensureTaskTarget(ctx context.Context, view TaskView) error {
	device, err := s.store.GetDevice(ctx, view.TargetDeviceID)
	if err != nil {
		return err
	}
	if err := requirePairedDevice(device); err != nil {
		return err
	}
	if device.ConnectionEpoch != view.TargetEpoch {
		return fmt.Errorf("task target epoch %d, device epoch %d: %w", view.TargetEpoch, device.ConnectionEpoch, ErrDeviceFence)
	}
	return nil
}

func requirePairedDevice(device Device) error {
	switch device.State {
	case DeviceStatePaired:
		return nil
	case DeviceStateUnreachable:
		return ErrDeviceUnreachable
	case DeviceStateRevoked:
		return ErrDeviceRevoked
	default:
		return ErrDeviceNotPaired
	}
}

func (s *Service) transition(ctx context.Context, view TaskView, to workmodel.State, eventType string, payload any) (TaskView, Event, error) {
	if !workmodel.CanTransition(view.State, to) {
		return TaskView{}, Event{}, fmt.Errorf("transition %s to %s: %w", view.State, to, store.ErrInvalidTransition)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return TaskView{}, Event{}, err
	}
	now := s.clock().UTC()
	workEvent := workmodel.Event{ID: s.newID("event"), TaskID: view.ID, Type: workmodel.EventStateTransitioned, Visibility: workmodel.VisibilityUser, Payload: encoded, CreatedAt: now}
	local := localEvent(s.newID("event"), "task", view.ID, view.ID, view.Revision+1, eventType, payload, now)
	storedEvent, err := s.store.TransitionAtRevision(ctx, view.ID, view.Revision, to, workEvent, local)
	if err != nil {
		return TaskView{}, Event{}, err
	}
	next, err := s.taskView(ctx, view.ID)
	if err != nil {
		return TaskView{}, Event{}, err
	}
	return next, storedEvent, nil
}

func (s *Service) replay(ctx context.Context, key, operation string, payload any, destination any) (bool, error) {
	if err := validateIdempotencyKey(key); err != nil {
		return false, err
	}
	record, err := s.store.LoadIdempotency(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if record.Operation != operation || record.RequestHash != requestHash(operation, payload) {
		return false, ErrIdempotencyConflict
	}
	if err := json.Unmarshal(record.ResponseBytes, destination); err != nil {
		return false, fmt.Errorf("decode idempotent response: %w", err)
	}
	return true, nil
}

type deviceReplayContextKey struct{}

type deviceReplayState struct {
	requestHash string
}

func withDeviceReplay(ctx context.Context, requestHash string) context.Context {
	return context.WithValue(ctx, deviceReplayContextKey{}, deviceReplayState{requestHash: requestHash})
}

func isDeviceReplay(ctx context.Context) bool {
	_, ok := ctx.Value(deviceReplayContextKey{}).(deviceReplayState)
	return ok
}

func deviceReplayRequestHash(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(deviceReplayContextKey{}).(deviceReplayState)
	return value.requestHash, ok && value.requestHash != ""
}

func (s *Service) replayAction(ctx context.Context, key, operation string, payload any, destination any) (bool, error) {
	if isDeviceReplay(ctx) {
		return false, nil
	}
	return s.replay(ctx, key, operation, payload, destination)
}

func (s *Service) remember(ctx context.Context, key, operation string, payload, response any) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return s.rememberWithHash(ctx, key, operation, requestHash(operation, payload), encoded)
}

func (s *Service) rememberWithHash(ctx context.Context, key, operation, hash string, encoded []byte) error {
	record := IdempotencyRecord{Key: key, Operation: operation, RequestHash: hash, ResponseBytes: encoded, CreatedAt: s.clock().UTC()}
	if err := s.store.SaveIdempotency(ctx, record); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return ErrIdempotencyConflict
		}
		return err
	}
	return nil
}

func (s *Service) rememberAction(ctx context.Context, key, operation string, payload, response any) error {
	if hash, ok := deviceReplayRequestHash(ctx); ok {
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		return s.rememberWithHash(ctx, key, operation, hash, encoded)
	}
	return s.remember(ctx, key, operation, payload, response)
}

func localEvent(id, resourceType, resourceID, taskID string, revision int64, eventType string, payload any, created time.Time) Event {
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) == 0 {
		encoded = []byte(`{}`)
	}
	return Event{ID: id, ResourceType: resourceType, ResourceID: resourceID, TaskID: taskID, Revision: revision, Type: eventType, Payload: encoded, CreatedAt: created.UTC()}
}

func (s *Service) latestLocalEvent(ctx context.Context, taskID, eventType string) (Event, error) {
	values, err := s.store.ListLocalEvents(ctx, taskID, 0, 200)
	if err != nil {
		return Event{}, err
	}
	for index := len(values) - 1; index >= 0; index-- {
		if values[index].Type == eventType {
			return values[index], nil
		}
	}
	return Event{}, store.ErrNotFound
}

func checkRevision(view TaskView, expected int64) error {
	if view.Revision != expected {
		return fmt.Errorf("task revision %d, expected %d: %w", view.Revision, expected, ErrStaleRevision)
	}
	return nil
}

func validateIdempotencyKey(value string) error {
	if !validID(value) {
		return fmt.Errorf("idempotency key: %w", ErrInvalidRequest)
	}
	return nil
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func normalizeName(value string) string { return strings.Join(strings.Fields(value), " ") }

func validateRemote(value string) error {
	if value == "" || strings.ContainsAny(value, "\x00\r\n\\") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") || strings.HasPrefix(strings.ToLower(value), "file:") || value == "." || value == ".." || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return fmt.Errorf("repository remote: %w", ErrInvalidRequest)
	}
	if parsed, err := url.Parse(value); err == nil {
		if parsed.Scheme != "" && parsed.Host == "" && parsed.Scheme != "ssh" {
			return fmt.Errorf("repository remote: %w", ErrInvalidRequest)
		}
		if parsed.Scheme == "" && !strings.Contains(value, ":") && strings.Contains(value, "/") {
			return fmt.Errorf("repository remote: %w", ErrInvalidRequest)
		}
	}
	return nil
}

func requestHash(operation string, payload any) string {
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(append([]byte(operation+"\x00"), encoded...))
	return hex.EncodeToString(digest[:])
}

func defaultID(prefix string) string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(value)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 256 {
		return message[:256]
	}
	return message
}
