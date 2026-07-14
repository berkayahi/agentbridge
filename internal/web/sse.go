package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/gofiber/fiber/v3"
)

func (s *Server) taskStream(c fiber.Ctx) error {
	taskID := c.Params("id")
	if _, err := s.deps.Store.Task(c.Context(), taskID); errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound)
	} else if err != nil {
		return err
	}
	lastID := c.Get("Last-Event-ID")
	if lastID == "" {
		lastID = c.Query("last_event_id")
	}
	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderConnection, "keep-alive")
	return c.SendStreamWriter(func(writer *bufio.Writer) {
		ctx := c.Context()
		subscription := s.deps.Live.Subscribe(ctx)
		defer subscription.Cancel()

		replay, err := s.deps.Store.Events(ctx, taskID)
		if err != nil {
			return
		}
		seen := make(map[string]struct{}, len(replay))
		started := lastID == ""
		foundLast := lastID == ""
		for _, event := range replay {
			if event.Visibility != task.VisibilityUser {
				continue
			}
			if !started {
				if event.ID == lastID {
					started = true
					foundLast = true
				}
				continue
			}
			if event.ID == lastID {
				continue
			}
			if writeSSE(writer, event.ID, string(event.Type), event.Payload) != nil {
				return
			}
			seen[event.ID] = struct{}{}
		}
		if !foundLast {
			_ = writeSSE(writer, "", "reset", []byte(`{"reason":"replay_unavailable"}`))
			_ = writer.Flush()
			return
		}
		if writer.Flush() != nil {
			return
		}

		keepalive := time.NewTicker(s.config.KeepAlive)
		defer keepalive.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepalive.C:
				if _, err := writer.WriteString(": keepalive\n\n"); err != nil || writer.Flush() != nil {
					return
				}
			case delivery, ok := <-subscription.C:
				if !ok || delivery.Dropped > 0 {
					return
				}
				if _, duplicate := seen[delivery.Event.ID]; duplicate {
					continue
				}
				var event task.Event
				if json.Unmarshal(delivery.Event.Payload, &event) != nil || event.TaskID != taskID || event.Visibility != task.VisibilityUser {
					continue
				}
				if writeSSE(writer, event.ID, string(event.Type), event.Payload) != nil || writer.Flush() != nil {
					return
				}
				seen[event.ID] = struct{}{}
			}
		}
	})
}

func writeSSE(writer *bufio.Writer, id, eventType string, payload []byte) error {
	if strings.ContainsAny(id, "\r\n") || strings.ContainsAny(eventType, "\r\n") {
		return fmt.Errorf("invalid SSE metadata")
	}
	if _, err := fmt.Fprintf(writer, "id: %s\nevent: %s\n", id, eventType); err != nil {
		return err
	}
	for _, line := range strings.Split(string(payload), "\n") {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := writer.WriteString("\n")
	return err
}

// mergeReplay is the deterministic core of replay/live gap handling. Live
// events already present in replay are skipped; a dropped delivery forces the
// connection to close so EventSource reconnects from its last event ID.
func mergeReplay(replay []task.Event, live []events.Delivery, lastID string) ([]string, bool) {
	seen := make(map[string]struct{}, len(replay))
	result := make([]string, 0, len(replay)+len(live))
	started := lastID == ""
	foundLast := lastID == ""
	for _, event := range replay {
		seen[event.ID] = struct{}{}
		if !started {
			if event.ID == lastID {
				started = true
				foundLast = true
			}
			continue
		}
		if event.ID != lastID {
			result = append(result, event.ID)
		}
	}
	if !foundLast {
		return nil, true
	}
	for _, delivery := range live {
		if delivery.Dropped > 0 {
			return result, true
		}
		if _, duplicate := seen[delivery.Event.ID]; duplicate {
			continue
		}
		seen[delivery.Event.ID] = struct{}{}
		result = append(result, delivery.Event.ID)
	}
	return result, false
}
