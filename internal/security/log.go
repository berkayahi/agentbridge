package security

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

var logURLPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// LogHandler redacts every message and structured attribute before delegating
// to a concrete slog handler. Arbitrary values are rendered as bounded text so
// nested errors or structs cannot bypass redaction through slog.KindAny.
type LogHandler struct {
	next     slog.Handler
	redactor *Redactor
}

func NewLogHandler(next slog.Handler, redactor *Redactor) *LogHandler {
	if next == nil {
		next = slog.DiscardHandler
	}
	if redactor == nil {
		redactor = NewRedactor(Config{})
	}
	return &LogHandler{next: next, redactor: redactor}
}

func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *LogHandler) Handle(ctx context.Context, record slog.Record) error {
	redacted := slog.NewRecord(record.Time, record.Level, h.text(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		redacted.AddAttrs(h.attr(attr))
		return true
	})
	return h.next.Handle(ctx, redacted)
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		redacted[i] = h.attr(attr)
	}
	return &LogHandler{next: h.next.WithAttrs(redacted), redactor: h.redactor}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{next: h.next.WithGroup(h.text(name)), redactor: h.redactor}
}

func (h *LogHandler) attr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.String(attr.Key, h.text(value.String()))
	case slog.KindGroup:
		group := value.Group()
		for i := range group {
			group[i] = h.attr(group[i])
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(group...)}
	case slog.KindAny:
		if value.Any() == nil {
			return attr
		}
		return slog.String(attr.Key, h.text(fmt.Sprint(value.Any())))
	default:
		return slog.Attr{Key: attr.Key, Value: value}
	}
}

func (h *LogHandler) text(value string) string {
	return logURLPattern.ReplaceAllString(h.redactor.RedactString(value), "[REDACTED:url]")
}

var _ slog.Handler = (*LogHandler)(nil)
