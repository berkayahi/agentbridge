package localcontrol

import (
	"context"
	"strings"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
)

const maxListLimit = 200

// ListProjects reports every project in the hive.
func (s *Service) ListProjects(ctx context.Context) (ProjectsResponse, error) {
	authority, err := s.listing()
	if err != nil {
		return ProjectsResponse{}, err
	}
	projects, err := authority.ListProjects(ctx)
	if err != nil {
		return ProjectsResponse{}, err
	}
	if projects == nil {
		projects = []Project{}
	}
	return ProjectsResponse{Projects: projects}, nil
}

// ListBoards reports the boards of one project.
func (s *Service) ListBoards(ctx context.Context, projectID string) (BoardsResponse, error) {
	authority, err := s.listing()
	if err != nil {
		return BoardsResponse{}, err
	}
	if !validID(projectID) {
		return BoardsResponse{}, ErrInvalidRequest
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return BoardsResponse{}, err
	}
	boards, err := authority.ListBoards(ctx, projectID)
	if err != nil {
		return BoardsResponse{}, err
	}
	if boards == nil {
		boards = []Board{}
	}
	return BoardsResponse{Boards: boards}, nil
}

// ListTasks reports this controller's tasks. The listing is scoped to locally
// controlled tasks: a task owned by the standalone controller would refuse every
// action a client could offer on it, so showing it would be a false promise.
func (s *Service) ListTasks(ctx context.Context, filter TaskFilter) (TasksResponse, error) {
	authority, err := s.listing()
	if err != nil {
		return TasksResponse{}, err
	}
	for _, id := range []string{filter.ProjectID, filter.BoardID, filter.RepositoryID, filter.TargetDeviceID} {
		if strings.TrimSpace(id) != "" && !validID(id) {
			return TasksResponse{}, ErrInvalidRequest
		}
	}
	limit := filter.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	tasks, err := authority.ListTasks(ctx, store.ListFilter{
		RepoProfileID:   strings.TrimSpace(filter.RepositoryID),
		States:          filter.States,
		Limit:           limit,
		ControllerOwner: workmodel.TaskControllerLocal,
		ProjectID:       strings.TrimSpace(filter.ProjectID),
		BoardID:         strings.TrimSpace(filter.BoardID),
		TargetDeviceID:  strings.TrimSpace(filter.TargetDeviceID),
	})
	if err != nil {
		return TasksResponse{}, err
	}
	views := make([]TaskView, 0, len(tasks))
	for _, task := range tasks {
		// The view is assembled per task rather than joined into the listing
		// query, so one mapper serves both the listing and the single-task read
		// and they can never disagree about what a task is.
		view, err := s.taskView(ctx, task.ID)
		if err != nil {
			continue
		}
		views = append(views, view)
	}
	return TasksResponse{Tasks: views}, nil
}

// ObserveHive reads the whole local event log from a cursor, so one client poll
// serves every bee instead of one poll per bee.
func (s *Service) ObserveHive(ctx context.Context, after uint64, limit int) (HiveResponse, error) {
	authority, err := s.listing()
	if err != nil {
		return HiveResponse{}, err
	}
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}
	events, err := authority.ListLocalEventsSince(ctx, after, limit)
	if err != nil {
		return HiveResponse{}, err
	}
	if events == nil {
		events = []Event{}
	}
	response := HiveResponse{Events: events}
	if len(events) > 0 {
		response.NextCursor = events[len(events)-1].Cursor
	}
	return response, nil
}

func (s *Service) listing() (LocalListingAuthority, error) {
	if s == nil || s.store == nil {
		return nil, ErrNotConfigured
	}
	authority, ok := s.store.(LocalListingAuthority)
	if !ok {
		return nil, ErrNotConfigured
	}
	return authority, nil
}
