package codex

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/berkayahi/agentbridge/internal/provider"
)

func mapNotification(message ServerMessage, taskID provider.ID, now time.Time) (provider.Event, bool) {
	event := provider.Event{TaskID: taskID, CreatedAt: now}
	switch message.Method {
	case "item/agentMessage/delta":
		var params struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return provider.Event{}, false
		}
		event.Type, event.Message = provider.EventAssistantMessage, params.Delta
	case "item/started", "item/completed":
		var params struct {
			Item struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Path    string `json:"path"`
				Name    string `json:"name"`
			} `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return provider.Event{}, false
		}
		started := message.Method == "item/started"
		switch params.Item.Type {
		case "commandExecution":
			if started {
				event.Type = provider.EventCommandStarted
			} else {
				event.Type = provider.EventCommandEnded
			}
			event.Message = params.Item.Command
		case "fileChange":
			if started {
				event.Type = provider.EventFileStarted
			} else {
				event.Type = provider.EventFileEnded
			}
			event.Path = params.Item.Path
		default:
			if started {
				event.Type = provider.EventToolStarted
			} else {
				event.Type = provider.EventToolEnded
			}
			event.Tool = params.Item.Name
		}
	case "turn/completed":
		event.Type = provider.EventCompleted
	case "error":
		var params struct {
			Error struct {
				Message string          `json:"message"`
				Info    json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(message.Params, &params) != nil {
			return provider.Event{}, false
		}
		event.Message = params.Error.Message
		info := strings.ToLower(string(params.Error.Info))
		switch {
		case strings.Contains(info, "unauthorized"):
			event.Type = provider.EventAuthRequired
		case strings.Contains(info, "usagelimit") || strings.Contains(info, "sessionbudget"):
			event.Type = provider.EventRateLimited
		case params.WillRetry:
			// The provider is retrying this itself, so the turn has not failed
			// and the session is still alive. Reporting it as an error ends a
			// bee over a hiccup and claims a failure that never happened; the
			// keeper still sees the hiccup, as liveness rather than a death.
			event.Type = provider.EventHeartbeat
		default:
			event.Type = provider.EventError
		}
	default:
		return provider.Event{}, false
	}
	return event, true
}

func extractThreadID(params json.RawMessage) string {
	var value struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &value)
	return value.ThreadID
}
