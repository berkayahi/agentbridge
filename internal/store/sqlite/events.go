package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/berkayahi/agentbridge/internal/events"
	"github.com/berkayahi/agentbridge/internal/store"
)

type eventRepository struct{ db v2Querier }

func (r eventRepository) Append(ctx context.Context, value events.Record) error {
	var providerID any
	if value.ProviderEventID != "" {
		providerID = value.ProviderEventID
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO execution_events (id, execution_id, event_type, visibility, provider_event_id, redacted_payload, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, value.ID, value.ExecutionID, value.Type, value.Visibility, providerID, append([]byte(nil), value.Payload...), timestamp(value.CreatedAt))
	if err != nil {
		if stringsContains(err.Error(), "execution_events") && stringsContains(err.Error(), "UNIQUE") {
			return fmt.Errorf("append execution event: %w", store.ErrDuplicateEvent)
		}
		return v2Conflict("append execution event", err)
	}
	return nil
}

func (r eventRepository) List(ctx context.Context, executionID string) ([]events.Record, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, execution_id, event_type, visibility, provider_event_id, redacted_payload, created_at FROM execution_events WHERE execution_id = ? ORDER BY created_at, id`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list execution events: %w", err)
	}
	defer rows.Close()
	var values []events.Record
	for rows.Next() {
		var value events.Record
		var providerID sql.NullString
		var payload, created string
		if err := rows.Scan(&value.ID, &value.ExecutionID, &value.Type, &value.Visibility, &providerID, &payload, &created); err != nil {
			return nil, fmt.Errorf("scan execution event: %w", err)
		}
		value.ProviderEventID, value.Payload = providerID.String, append([]byte(nil), []byte(payload)...)
		value.CreatedAt, err = parseTimestamp(created)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func stringsContains(value, part string) bool {
	return len(value) >= len(part) && stringIndex(value, part) >= 0
}

func stringIndex(value, part string) int {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}

var _ events.Repository = eventRepository{}
