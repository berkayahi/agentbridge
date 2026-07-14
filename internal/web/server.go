// Package web exposes the private, Tailscale Serve-only AgentBridge dashboard.
package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/gofiber/fiber/v3"
)

const tailscaleLoginHeader = "Tailscale-User-Login"

type ReadStore interface {
	Task(context.Context, string) (task.Task, error)
	ListTasks(context.Context, store.ListFilter) ([]task.Task, error)
	Events(context.Context, string) ([]task.Event, error)
	Attachments(context.Context, string) ([]task.Attachment, error)
}

type Health struct {
	Status        string         `json:"status"`
	Version       string         `json:"version,omitempty"`
	QueueDepth    int            `json:"queue_depth,omitempty"`
	ActiveTasks   int            `json:"active_tasks,omitempty"`
	DiskFreeBytes int64          `json:"disk_free_bytes,omitempty"`
	Components    map[string]any `json:"components,omitempty"`
}

type HealthSource interface {
	Health(context.Context) (Health, error)
}

type ProviderUsage struct {
	Provider    string    `json:"provider"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitempty"`
}

type UsageSource interface {
	Usage(context.Context) ([]ProviderUsage, error)
}

type AttachmentContent interface {
	Read(context.Context, task.Attachment) ([]byte, error)
}

type RecoveryView struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	State     string    `json:"state"`
	Prompt    string    `json:"prompt,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type RecoveryService interface {
	Start(context.Context, string, string) (RecoveryView, error)
	Inspect(context.Context, string, string, string) (RecoveryView, error)
	Submit(context.Context, string, string, string, string) (RecoveryView, error)
	Cancel(context.Context, string, string, string) error
}

type Config struct {
	AllowedIdentities []string
	ServeMode         bool
	CSRFSecret        []byte
	Now               func() time.Time
	KeepAlive         time.Duration
}

type Dependencies struct {
	Store    ReadStore
	Health   HealthSource
	Usage    UsageSource
	Recovery RecoveryService
	Live     *events.Bus
	Content  AttachmentContent
}

type Server struct {
	app      *fiber.App
	config   Config
	deps     Dependencies
	allowed  map[string]struct{}
	csrf     *csrfTokens
	views    *viewRenderer
	peerIP   func(any) string
	scheme   func(any) string
	identity func(any) string
}

func New(config Config, dependencies Dependencies) (*Server, error) {
	if dependencies.Store == nil || dependencies.Health == nil || dependencies.Usage == nil || dependencies.Recovery == nil || dependencies.Live == nil {
		return nil, errors.New("web: incomplete dependencies")
	}
	if !config.ServeMode || len(config.AllowedIdentities) == 0 || len(config.CSRFSecret) < 32 {
		return nil, errors.New("web: unsafe Serve configuration")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.KeepAlive <= 0 {
		config.KeepAlive = 15 * time.Second
	}
	allowed := make(map[string]struct{}, len(config.AllowedIdentities))
	for _, identity := range config.AllowedIdentities {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			return nil, errors.New("web: empty Tailscale identity")
		}
		allowed[identity] = struct{}{}
	}
	s := &Server{config: config, deps: dependencies, allowed: allowed, csrf: newCSRFTokens(config.CSRFSecret, config.Now)}
	views, err := newViewRenderer()
	if err != nil {
		return nil, err
	}
	s.views = views
	s.peerIP = func(value any) string {
		ctx, ok := value.(fiber.Ctx)
		if !ok || !ctx.IsFromLocal() {
			return ""
		}
		return "127.0.0.1"
	}
	s.scheme = func(value any) string {
		ctx, ok := value.(fiber.Ctx)
		if !ok {
			return ""
		}
		return ctx.Protocol()
	}
	s.identity = func(value any) string {
		ctx, ok := value.(fiber.Ctx)
		if !ok {
			return ""
		}
		return strings.TrimSpace(ctx.Get(tailscaleLoginHeader))
	}
	s.app = fiber.New(fiber.Config{
		TrustProxy:  true,
		ProxyHeader: fiber.HeaderXForwardedFor,
		TrustProxyConfig: fiber.TrustProxyConfig{
			Loopback: true,
		},
		ErrorHandler: func(c fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) {
				code = fiberErr.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": httpStatusMessage(code)})
		},
	})
	s.routes()
	return s, nil
}

func (s *Server) App() *fiber.App { return s.app }

func (s *Server) routes() {
	// The daemon binds to a validated loopback-only address. Keep this probe
	// outside Tailscale identity middleware so systemd can distinguish a live
	// process from a dashboard authentication failure.
	s.app.Get("/healthz", func(c fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
		return c.SendString("ok\n")
	})
	s.app.Use(s.securityMiddleware)
	s.app.Get("/", s.overviewPage)
	s.app.Get("/tasks/:id", s.taskPage)
	s.app.Get("/auth/:provider", s.authPage)
	s.app.Get("/assets/app.js", s.javascriptAsset)
	s.app.Get("/assets/styles.css", s.stylesheetAsset)
	api := s.app.Group("/api/v1")
	api.Get("/health", s.health)
	api.Get("/tasks", s.tasks)
	api.Get("/tasks/:id", s.task)
	api.Get("/tasks/:id/events", s.taskEvents)
	api.Get("/tasks/:id/attachments", s.taskAttachments)
	api.Get("/tasks/:id/attachments/:attachment/content", s.taskAttachmentContent)
	api.Get("/tasks/:id/stream", s.taskStream)
	api.Get("/usage", s.usage)
	api.Get("/csrf", s.issueCSRF)
	api.Post("/auth/:provider/recovery", s.requireCSRF, s.startRecovery)
	api.Get("/auth/:provider/recovery/:id", s.inspectRecovery)
	api.Post("/auth/:provider/recovery/:id/input", s.requireCSRF, s.submitRecovery)
	api.Delete("/auth/:provider/recovery/:id", s.requireCSRF, s.cancelRecovery)
	api.All("/*", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusMethodNotAllowed)
	})
}

func httpStatusMessage(code int) string {
	switch code {
	case fiber.StatusBadRequest:
		return "bad request"
	case fiber.StatusForbidden:
		return "forbidden"
	case fiber.StatusNotFound:
		return "not found"
	case fiber.StatusMethodNotAllowed:
		return "method not allowed"
	default:
		return fmt.Sprintf("request failed (%d)", code)
	}
}
