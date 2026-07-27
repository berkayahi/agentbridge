package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/berkayahi/agentbridge/internal/localcontrol"
)

// TaskUsage attributes provider-reported per-turn token events to the durable
// local task context. A provider that emits only subscription-window usage does
// not acquire invented per-task numbers.
func (s *RuntimeStore) TaskUsage(ctx context.Context, projectID string) ([]localcontrol.TaskUsageView, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("task usage: runtime store is unavailable")
	}
	query := `
		SELECT e.id, e.event_type, e.payload, c.project_id, l.id, l.repo_profile_id, l.provider
		FROM local_control_events e
		JOIN local_tasks l ON l.id = e.local_task_id
		JOIN local_task_contexts c ON c.local_task_id = l.id
		WHERE e.event_type IN ('provider_event', 'device_event')`
	args := []any{}
	if strings.TrimSpace(projectID) != "" {
		query += ` AND c.project_id = ?`
		args = append(args, strings.TrimSpace(projectID))
	}
	query += ` ORDER BY e.cursor`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query task usage: %w", err)
	}
	defer rows.Close()

	type accumulator struct {
		view localcontrol.TaskUsageView
		seen map[string]struct{}
	}
	byTask := make(map[string]*accumulator)
	for rows.Next() {
		var eventID, eventType, project, taskID, repositoryID, providerID string
		var payload []byte
		if err := rows.Scan(&eventID, &eventType, &payload, &project, &taskID, &repositoryID, &providerID); err != nil {
			return nil, fmt.Errorf("scan task usage: %w", err)
		}
		turnID, tokens, ok := reportedTurnUsage(eventType, payload)
		if !ok {
			continue
		}
		if turnID == "" {
			turnID = eventID
		}
		value := byTask[taskID]
		if value == nil {
			value = &accumulator{
				view: localcontrol.TaskUsageView{
					TaskID: taskID, ProjectID: project, RepositoryID: repositoryID, Provider: providerID,
				},
				seen: make(map[string]struct{}),
			}
			byTask[taskID] = value
		}
		if _, duplicate := value.seen[turnID]; duplicate {
			continue
		}
		value.seen[turnID] = struct{}{}
		value.view.Turns++
		addTokenUsage(&value.view.Tokens, tokens)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read task usage: %w", err)
	}

	values := make([]localcontrol.TaskUsageView, 0, len(byTask))
	for _, value := range byTask {
		values = append(values, value.view)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Tokens.Total == values[j].Tokens.Total {
			return values[i].TaskID < values[j].TaskID
		}
		return values[i].Tokens.Total > values[j].Tokens.Total
	})
	return values, nil
}

func reportedTurnUsage(eventType string, payload []byte) (string, localcontrol.TokenUsageView, bool) {
	var carrier struct {
		EventType string          `json:"event_type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(payload, &carrier) != nil {
		return "", localcontrol.TokenUsageView{}, false
	}
	if eventType == "device_event" {
		return reportedTurnUsage(carrier.EventType, carrier.Payload)
	}
	if eventType != "provider_event" || carrier.EventType != "usage" {
		return "", localcontrol.TokenUsageView{}, false
	}
	var event struct {
		Usage *struct {
			TurnID string                       `json:"turn_id"`
			Tokens *localcontrol.TokenUsageView `json:"tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(carrier.Payload, &event) != nil || event.Usage == nil || event.Usage.Tokens == nil {
		return "", localcontrol.TokenUsageView{}, false
	}
	return event.Usage.TurnID, *event.Usage.Tokens, true
}

func addTokenUsage(total *localcontrol.TokenUsageView, value localcontrol.TokenUsageView) {
	total.Input += value.Input
	total.CachedInput += value.CachedInput
	total.Output += value.Output
	total.ReasoningOutput += value.ReasoningOutput
	total.Total += value.Total
}

var _ localcontrol.UsageAnalyticsAuthority = (*RuntimeStore)(nil)
