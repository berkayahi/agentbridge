package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
	"github.com/gofiber/fiber/v3"
)

//go:embed templates/*.html static/*
var webAssets embed.FS

type viewRenderer struct{ templates *template.Template }

func newViewRenderer() (*viewRenderer, error) {
	functions := template.FuncMap{
		"elapsed": func(milliseconds int64) string {
			duration := time.Duration(milliseconds) * time.Millisecond
			if duration < 0 {
				duration = 0
			}
			hours := int(duration / time.Hour)
			minutes := int(duration/time.Minute) % 60
			seconds := int(duration/time.Second) % 60
			if hours > 0 {
				return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, seconds)
			}
			return fmt.Sprintf("%dm %02ds", minutes, seconds)
		},
		"clock": func(value time.Time) string { return value.Local().Format("15:04:05") },
		"bytes": func(value int64) string {
			const gib = int64(1024 * 1024 * 1024)
			if value >= gib {
				return fmt.Sprintf("%.1f GiB", float64(value)/float64(gib))
			}
			return fmt.Sprintf("%d MiB", value/(1024*1024))
		},
		"jsontext": func(value json.RawMessage) string { return string(value) },
	}
	views, err := template.New("agentbridge").Funcs(functions).ParseFS(webAssets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse dashboard templates: %w", err)
	}
	return &viewRenderer{templates: views}, nil
}

type pageMeta struct{ Title, Page, Identity string }
type overviewView struct {
	Meta   pageMeta
	Health Health
	Tasks  []taskSummary
}
type taskPageView struct {
	Meta        pageMeta
	Task        taskDetail
	Timeline    []timelineView
	Attachments []attachmentView
}
type timelineView struct {
	ID        string
	Type      workmodel.EventType
	Message   string
	CreatedAt time.Time
}
type authPageView struct {
	Meta                   pageMeta
	Provider, ProviderName string
}

func (s *Server) overviewPage(c fiber.Ctx) error {
	health, err := s.deps.Health.Health(c.Context())
	if err != nil {
		return err
	}
	values, err := s.deps.Store.ListTasks(c.Context(), store.ListFilter{Limit: 50})
	if err != nil {
		return err
	}
	tasks := make([]taskSummary, 0, len(values))
	for _, value := range values {
		tasks = append(tasks, summarizeTask(value, s.config.Now()))
	}
	return s.render(c, "index", overviewView{Meta: s.meta(c, "Operations", "overview"), Health: health, Tasks: tasks})
}

func (s *Server) taskPage(c fiber.Ctx) error {
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
	attachments, err := s.deps.Store.Attachments(c.Context(), value.ID)
	if err != nil {
		return err
	}
	attachmentViews := make([]attachmentView, 0, len(attachments))
	for _, attachment := range attachments {
		attachmentViews = append(attachmentViews, attachmentView{ID: attachment.ID, Name: attachment.Name, MediaType: attachment.MediaType, SizeBytes: attachment.SizeBytes, CreatedAt: attachment.CreatedAt})
	}
	data := taskPageView{Meta: s.meta(c, value.Title, "task"), Task: detailTask(value, events, s.config.Now()), Timeline: visibleTimeline(events), Attachments: attachmentViews}
	return s.render(c, "task", data)
}

func (s *Server) authPage(c fiber.Ctx) error {
	provider, ok := recoveryProvider(c.Params("provider"))
	if !ok {
		return fiber.NewError(fiber.StatusNotFound)
	}
	name := strings.ToUpper(provider[:1]) + provider[1:]
	return s.render(c, "auth", authPageView{Meta: s.meta(c, name+" authentication", "auth"), Provider: provider, ProviderName: name})
}

func (s *Server) meta(c fiber.Ctx, title, page string) pageMeta {
	identity, _ := c.Locals("tailscale_identity").(string)
	return pageMeta{Title: title, Page: page, Identity: identity}
}
func (s *Server) render(c fiber.Ctx, name string, data any) error {
	c.Type("html", "utf-8")
	return s.views.templates.ExecuteTemplate(c.Response().BodyWriter(), name, data)
}
func visibleTimeline(values []workmodel.Event) []timelineView {
	result := make([]timelineView, 0, len(values))
	for _, value := range values {
		if value.Visibility != workmodel.VisibilityUser {
			continue
		}
		message := string(value.Payload)
		var payload struct{ Message, Summary string }
		if json.Unmarshal(value.Payload, &payload) == nil {
			if payload.Message != "" {
				message = payload.Message
			} else if payload.Summary != "" {
				message = payload.Summary
			}
		}
		result = append(result, timelineView{ID: value.ID, Type: value.Type, Message: message, CreatedAt: value.CreatedAt})
	}
	return result
}
func (s *Server) javascriptAsset(c fiber.Ctx) error {
	return serveEmbedded(c, "static/app.js", "text/javascript; charset=utf-8")
}
func (s *Server) stylesheetAsset(c fiber.Ctx) error {
	return serveEmbedded(c, "static/styles.css", "text/css; charset=utf-8")
}
func serveEmbedded(c fiber.Ctx, name, contentType string) error {
	content, err := fs.ReadFile(webAssets, name)
	if err != nil {
		return fiber.NewError(fiber.StatusNotFound)
	}
	c.Set(fiber.HeaderContentType, contentType)
	return c.Send(content)
}
