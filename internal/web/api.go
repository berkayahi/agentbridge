package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/gofiber/fiber/v3"
)

func (s *Server) health(c fiber.Ctx) error {
	value, err := s.deps.Health.Health(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(value)
}

func (s *Server) tasks(c fiber.Ctx) error {
	limit := parseLimit(c.Query("limit"), 20, 100)
	values, err := s.deps.Store.ListTasks(c.Context(), store.ListFilter{})
	if err != nil {
		return err
	}
	values = afterTaskCursor(values, c.Query("cursor"))
	next := ""
	if len(values) > limit {
		next = encodeCursor(values[limit-1].ID)
		values = values[:limit]
	}
	items := make([]taskSummary, 0, len(values))
	for _, value := range values {
		items = append(items, summarizeTask(value, s.config.Now()))
	}
	return c.JSON(fiber.Map{"items": items, "next_cursor": next})
}

func (s *Server) task(c fiber.Ctx) error {
	value, err := s.deps.Store.Task(c.Context(), c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound)
	}
	if err != nil {
		return err
	}
	events, err := s.deps.Store.Events(c.Context(), value.ID)
	if err != nil {
		return err
	}
	return c.JSON(detailTask(value, events, s.config.Now()))
}

func (s *Server) taskEvents(c fiber.Ctx) error {
	if _, err := s.deps.Store.Task(c.Context(), c.Params("id")); errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound)
	} else if err != nil {
		return err
	}
	values, err := s.deps.Store.Events(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	visible := make([]task.Event, 0, len(values))
	for _, value := range values {
		if value.Visibility == task.VisibilityUser {
			visible = append(visible, value)
		}
	}
	visible = afterEventCursor(visible, c.Query("cursor"))
	limit := parseLimit(c.Query("limit"), 100, 500)
	next := ""
	if len(visible) > limit {
		next = encodeCursor(visible[limit-1].ID)
		visible = visible[:limit]
	}
	return c.JSON(fiber.Map{"items": visible, "next_cursor": next})
}

func (s *Server) taskAttachments(c fiber.Ctx) error {
	if _, err := s.deps.Store.Task(c.Context(), c.Params("id")); errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound)
	} else if err != nil {
		return err
	}
	values, err := s.deps.Store.Attachments(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	items := make([]attachmentView, 0, len(values))
	for _, value := range values {
		items = append(items, attachmentView{ID: value.ID, Name: value.Name, MediaType: value.MediaType, SizeBytes: value.SizeBytes, CreatedAt: value.CreatedAt})
	}
	return c.JSON(fiber.Map{"items": items})
}

func (s *Server) taskAttachmentContent(c fiber.Ctx) error {
	if s.deps.Content == nil {
		return fiber.NewError(fiber.StatusNotFound)
	}
	values, err := s.deps.Store.Attachments(c.Context(), c.Params("id"))
	if err != nil {
		return err
	}
	var selected task.Attachment
	for _, value := range values {
		if value.ID == c.Params("attachment") {
			selected = value
			break
		}
	}
	if selected.ID == "" || selected.MediaType != "image/jpeg" && selected.MediaType != "image/png" && selected.MediaType != "image/webp" {
		return fiber.NewError(fiber.StatusNotFound)
	}
	if selected.SizeBytes < 0 || selected.SizeBytes > 20<<20 {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge)
	}
	content, err := s.deps.Content.Read(c.Context(), selected)
	if err != nil {
		return err
	}
	if int64(len(content)) != selected.SizeBytes {
		return fiber.NewError(fiber.StatusInternalServerError)
	}
	c.Set(fiber.HeaderContentType, selected.MediaType)
	c.Set(fiber.HeaderContentDisposition, `inline; filename="attachment"`)
	return c.Send(content)
}

func (s *Server) usage(c fiber.Ctx) error {
	values, err := s.deps.Usage.Usage(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"providers": values})
}

func (s *Server) startRecovery(c fiber.Ctx) error {
	provider, ok := recoveryProvider(c.Params("provider"))
	if !ok {
		return fiber.NewError(fiber.StatusNotFound)
	}
	identity, _ := c.Locals("tailscale_identity").(string)
	value, err := s.deps.Recovery.Start(c.Context(), provider, identity)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(value)
}

func (s *Server) inspectRecovery(c fiber.Ctx) error {
	provider, ok := recoveryProvider(c.Params("provider"))
	if !ok {
		return fiber.NewError(fiber.StatusNotFound)
	}
	identity, _ := c.Locals("tailscale_identity").(string)
	value, err := s.deps.Recovery.Inspect(c.Context(), provider, c.Params("id"), identity)
	if err != nil {
		return err
	}
	return c.JSON(value)
}

func (s *Server) submitRecovery(c fiber.Ctx) error {
	provider, ok := recoveryProvider(c.Params("provider"))
	if !ok {
		return fiber.NewError(fiber.StatusNotFound)
	}
	var input struct {
		Value string `json:"value"`
	}
	if len(c.Body()) > 0 {
		if err := c.Bind().Body(&input); err != nil {
			return fiber.NewError(fiber.StatusBadRequest)
		}
	}
	identity, _ := c.Locals("tailscale_identity").(string)
	value, err := s.deps.Recovery.Submit(c.Context(), provider, c.Params("id"), identity, input.Value)
	if err != nil {
		return err
	}
	return c.JSON(value)
}

func (s *Server) cancelRecovery(c fiber.Ctx) error {
	provider, ok := recoveryProvider(c.Params("provider"))
	if !ok {
		return fiber.NewError(fiber.StatusNotFound)
	}
	identity, _ := c.Locals("tailscale_identity").(string)
	if err := s.deps.Recovery.Cancel(c.Context(), provider, c.Params("id"), identity); err != nil {
		return err
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func recoveryProvider(value string) (string, bool) {
	value = strings.ToLower(value)
	return value, value == "codex" || value == "claude"
}

type taskSummary struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Repository string        `json:"repository"`
	Provider   task.Provider `json:"provider"`
	State      task.State    `json:"state"`
	ElapsedMS  int64         `json:"elapsed_ms"`
	UpdatedAt  time.Time     `json:"updated_at"`
	Deployment string        `json:"deployment_url,omitempty"`
	Failure    string        `json:"failure_reason,omitempty"`
}

type taskDetail struct {
	taskSummary
	BaseSHA   string       `json:"base_sha,omitempty"`
	CommitSHA string       `json:"commit_sha,omitempty"`
	PushRef   string       `json:"push_ref,omitempty"`
	Results   []resultView `json:"results"`
}

func summarizeTask(value task.Task, now time.Time) taskSummary {
	return taskSummary{ID: value.ID, Title: value.Title, Repository: value.RepoProfileID, Provider: value.Provider, State: value.State, ElapsedMS: value.Elapsed(now).Milliseconds(), UpdatedAt: value.UpdatedAt, Deployment: value.DeploymentURL, Failure: value.FailureReason}
}
func detailTask(value task.Task, events []task.Event, now time.Time) taskDetail {
	results := make([]resultView, 0)
	for _, event := range events {
		if event.Visibility != task.VisibilityUser || !resultEvent(event.Type) || !json.Valid(event.Payload) {
			continue
		}
		results = append(results, resultView{Type: event.Type, Payload: append(json.RawMessage(nil), event.Payload...), CreatedAt: event.CreatedAt})
	}
	return taskDetail{taskSummary: summarizeTask(value, now), BaseSHA: value.BaseSHA, CommitSHA: value.CommitSHA, PushRef: value.PushRef, Results: results}
}

type resultView struct {
	Type      task.EventType  `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

func resultEvent(eventType task.EventType) bool {
	switch eventType {
	case task.EventDiffSummary, task.EventVerification, task.EventCommitCreated, task.EventPushCompleted, task.EventDeployment:
		return true
	default:
		return false
	}
}

type attachmentView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

func parseLimit(raw string, fallback, maximum int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
func encodeCursor(id string) string { return base64.RawURLEncoding.EncodeToString([]byte(id)) }
func decodeCursor(value string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return ""
	}
	return string(decoded)
}
func afterTaskCursor(values []task.Task, cursor string) []task.Task {
	id := decodeCursor(cursor)
	if id == "" {
		return values
	}
	for index, value := range values {
		if value.ID == id {
			return values[index+1:]
		}
	}
	return nil
}
func afterEventCursor(values []task.Event, cursor string) []task.Event {
	id := decodeCursor(cursor)
	if id == "" {
		return values
	}
	for index, value := range values {
		if value.ID == id {
			return values[index+1:]
		}
	}
	return nil
}
