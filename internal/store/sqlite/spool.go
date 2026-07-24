package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/berkayahi/agentbridge/internal/spool"
)

var (
	spoolSchemaMu sync.Mutex
	spoolWriteMu  sync.Mutex
)

const spoolSchema = `
CREATE TABLE IF NOT EXISTS spool_messages (
    message_id INTEGER PRIMARY KEY,
    execution_id TEXT NOT NULL,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    lane TEXT NOT NULL,
    event_type TEXT NOT NULL,
    provider_event_id TEXT NOT NULL DEFAULT '',
    coalesce_key TEXT NOT NULL DEFAULT '',
    payload BLOB NOT NULL,
    payload_hash TEXT NOT NULL,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    created_at TEXT NOT NULL,
    UNIQUE (execution_id, sequence)
);
CREATE UNIQUE INDEX IF NOT EXISTS spool_messages_provider_event_idx
    ON spool_messages (execution_id, provider_event_id)
    WHERE provider_event_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS spool_messages_coalesce_idx
    ON spool_messages (coalesce_key)
    WHERE coalesce_key <> '';
CREATE TABLE IF NOT EXISTS spool_sequences (
    execution_id TEXT PRIMARY KEY,
    next_sequence INTEGER NOT NULL CHECK (next_sequence > 0)
);
CREATE TABLE IF NOT EXISTS spool_receipts (
    receipt_id TEXT PRIMARY KEY,
    message_id INTEGER NOT NULL CHECK (message_id > 0),
    payload_hash TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    received_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spool_state (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    next_message_id INTEGER NOT NULL CHECK (next_message_id > 0),
    acknowledged_through INTEGER NOT NULL CHECK (acknowledged_through >= 0),
    bytes_used INTEGER NOT NULL CHECK (bytes_used >= 0)
);
INSERT OR IGNORE INTO spool_state (id, next_message_id, acknowledged_through, bytes_used)
VALUES (1, 1, 0, 0);
`

type spoolState struct {
	nextMessageID       uint64
	acknowledgedThrough uint64
	bytesUsed           int64
}

type spoolScanner interface {
	Scan(...any) error
}

// Spool returns the durable spool backend implemented by this SQLite store.
func (s *RuntimeStore) Spool() spool.Store { return s }

func NewSpoolService(s *RuntimeStore, config spool.Config) (*spool.Service, error) {
	if s == nil {
		return nil, spool.ErrInvalid
	}
	return spool.NewService(s, config)
}

func (s *RuntimeStore) Append(ctx context.Context, request spool.AppendRequest) (spool.AppendResult, error) {
	if s == nil || s.db == nil {
		return spool.AppendResult{}, spool.ErrInvalid
	}
	event, err := request.Event.Normalize(time.Now().UTC())
	if err != nil {
		return spool.AppendResult{}, err
	}
	config := request.Config.Normalize()
	if err := config.Validate(); err != nil {
		return spool.AppendResult{}, err
	}
	if err := ensureSpoolSchema(ctx, s.db); err != nil {
		return spool.AppendResult{}, err
	}

	spoolWriteMu.Lock()
	defer spoolWriteMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return spool.AppendResult{}, fmt.Errorf("begin spool append: %w", err)
	}
	defer tx.Rollback()
	result, err := appendSpoolTx(ctx, tx, event, config)
	if err != nil {
		return spool.AppendResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return spool.AppendResult{}, fmt.Errorf("commit spool append: %w", err)
	}
	return result, nil
}

func appendSpoolTx(ctx context.Context, tx *sql.Tx, event spool.Event, config spool.Config) (spool.AppendResult, error) {
	payloadHash := hashPayload(event.Payload)
	if event.ProviderEventID != "" {
		row := tx.QueryRowContext(ctx, spoolMessageQuery+" WHERE execution_id = ? AND provider_event_id = ?", event.ExecutionID, event.ProviderEventID)
		value, err := scanSpoolMessage(row)
		if err == nil {
			if value.PayloadHash != payloadHash {
				return spool.AppendResult{}, spool.ErrDuplicatePayload
			}
			return spool.AppendResult{Message: value, Duplicate: true}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return spool.AppendResult{}, fmt.Errorf("check duplicate spool event: %w", err)
		}
	}

	state, err := loadSpoolState(ctx, tx)
	if err != nil {
		return spool.AppendResult{}, err
	}
	size := int64(len(event.Payload))
	projected, overflow := addBytes(state.bytesUsed, size)
	if overflow {
		return spool.AppendResult{}, spool.ErrCriticalReserve
	}
	nonCriticalLimit := config.MaxBytes - config.CriticalReserveBytes
	if event.Lane == spool.LaneDiagnostic && (state.bytesUsed >= config.WarningWatermarkBytes || projected > config.WarningWatermarkBytes || projected > nonCriticalLimit) {
		return appendDiagnosticTruncationTx(ctx, tx, &state, event, config)
	}
	if event.Lane != spool.LaneCritical && (state.bytesUsed >= config.CriticalWatermarkBytes || projected > config.CriticalWatermarkBytes || projected > nonCriticalLimit) {
		return spool.AppendResult{}, spool.ErrSpoolPaused
	}
	if projected > config.MaxBytes {
		return spool.AppendResult{}, spool.ErrCriticalReserve
	}
	value, err := appendFreshMessageTx(ctx, tx, &state, event, payloadHash, size)
	if err != nil {
		return spool.AppendResult{}, err
	}
	return spool.AppendResult{Message: value}, nil
}

func appendDiagnosticTruncationTx(ctx context.Context, tx *sql.Tx, state *spoolState, event spool.Event, config spool.Config) (spool.AppendResult, error) {
	key := "diagnostic:" + event.ExecutionID
	var existing spool.Message
	row := tx.QueryRowContext(ctx, spoolMessageQuery+" WHERE coalesce_key = ?", key)
	value, err := scanSpoolMessage(row)
	if err == nil {
		payload := updateTruncationPayload(value.Payload, event.Type, int64(len(event.Payload)))
		delta := int64(len(payload)) - value.SizeBytes
		projected, overflow := addBytes(state.bytesUsed, delta)
		if overflow || projected > config.MaxBytes {
			return spool.AppendResult{}, spool.ErrCriticalReserve
		}
		hash := hashPayload(payload)
		if _, err := tx.ExecContext(ctx, `UPDATE spool_messages SET payload = ?, payload_hash = ?, size_bytes = ?, created_at = ? WHERE message_id = ?`, payload, hash, len(payload), timestamp(event.CreatedAt), value.MessageID); err != nil {
			return spool.AppendResult{}, fmt.Errorf("coalesce diagnostic truncation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE spool_state SET bytes_used = ? WHERE id = 1`, projected); err != nil {
			return spool.AppendResult{}, fmt.Errorf("update spool truncation usage: %w", err)
		}
		existing.Payload = append([]byte(nil), payload...)
		existing.PayloadHash = hash
		existing.SizeBytes = int64(len(payload))
		existing.CreatedAt = event.CreatedAt
		return spool.AppendResult{Message: existing, Truncated: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spool.AppendResult{}, fmt.Errorf("find diagnostic truncation marker: %w", err)
	}
	markerPayload := truncationPayload(event.Type, int64(len(event.Payload)))
	marker := spool.Event{ExecutionID: event.ExecutionID, Lane: spool.LaneCritical, Type: "diagnostic_truncated", CoalesceKey: key, Payload: markerPayload, CreatedAt: event.CreatedAt}
	markerSize := int64(len(markerPayload))
	projected, overflow := addBytes(state.bytesUsed, markerSize)
	if overflow || projected > config.MaxBytes {
		return spool.AppendResult{}, spool.ErrCriticalReserve
	}
	value, err = appendFreshMessageTx(ctx, tx, state, marker, hashPayload(markerPayload), markerSize)
	if err != nil {
		return spool.AppendResult{}, err
	}
	return spool.AppendResult{Message: value, Truncated: true}, nil
}

type truncationSummary struct {
	Type  string `json:"type"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

func truncationPayload(eventType string, size int64) []byte {
	value, _ := json.Marshal(truncationSummary{Type: eventType, Count: 1, Bytes: size})
	return value
}

func updateTruncationPayload(payload []byte, eventType string, size int64) []byte {
	var value truncationSummary
	if err := json.Unmarshal(payload, &value); err != nil {
		value = truncationSummary{Type: eventType}
	}
	if value.Type == "" {
		value.Type = eventType
	}
	value.Count++
	value.Bytes += size
	result, _ := json.Marshal(value)
	return result
}

func appendFreshMessageTx(ctx context.Context, tx *sql.Tx, state *spoolState, event spool.Event, payloadHash string, size int64) (spool.Message, error) {
	if state.nextMessageID == 0 || state.nextMessageID > math.MaxInt64 {
		return spool.Message{}, spool.ErrCriticalReserve
	}
	nextSequence, err := nextSpoolSequence(ctx, tx, event.ExecutionID)
	if err != nil {
		return spool.Message{}, err
	}
	messageID := state.nextMessageID
	if _, err := tx.ExecContext(ctx, `INSERT INTO spool_messages (message_id, execution_id, sequence, lane, event_type, provider_event_id, coalesce_key, payload, payload_hash, size_bytes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, messageID, event.ExecutionID, nextSequence, event.Lane, event.Type, event.ProviderEventID, event.CoalesceKey, event.Payload, payloadHash, size, timestamp(event.CreatedAt)); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "spool_messages_provider_event_idx") {
			return spool.Message{}, spool.ErrDuplicatePayload
		}
		return spool.Message{}, fmt.Errorf("insert spool message: %w", err)
	}
	state.nextMessageID++
	state.bytesUsed += size
	if _, err := tx.ExecContext(ctx, `UPDATE spool_state SET next_message_id = ?, bytes_used = ? WHERE id = 1`, state.nextMessageID, state.bytesUsed); err != nil {
		return spool.Message{}, fmt.Errorf("advance spool state: %w", err)
	}
	return spool.Message{MessageID: messageID, ExecutionID: event.ExecutionID, Sequence: nextSequence, Lane: event.Lane, Type: event.Type, ProviderEventID: event.ProviderEventID, Payload: append([]byte(nil), event.Payload...), PayloadHash: payloadHash, SizeBytes: size, CreatedAt: event.CreatedAt}, nil
}

func nextSpoolSequence(ctx context.Context, tx *sql.Tx, executionID string) (uint64, error) {
	var next int64
	err := tx.QueryRowContext(ctx, `SELECT next_sequence FROM spool_sequences WHERE execution_id = ?`, executionID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO spool_sequences (execution_id, next_sequence) VALUES (?, 2)`, executionID); err != nil {
			return 0, fmt.Errorf("initialize spool sequence: %w", err)
		}
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load spool sequence: %w", err)
	}
	if next <= 0 || next > math.MaxInt64 {
		return 0, spool.ErrCriticalReserve
	}
	if _, err := tx.ExecContext(ctx, `UPDATE spool_sequences SET next_sequence = ? WHERE execution_id = ?`, next+1, executionID); err != nil {
		return 0, fmt.Errorf("advance spool sequence: %w", err)
	}
	return uint64(next), nil
}

func (s *RuntimeStore) Replay(ctx context.Context, request spool.ReplayRequest) ([]spool.Message, error) {
	if s == nil || s.db == nil {
		return nil, spool.ErrInvalid
	}
	if request.AfterMessageID > math.MaxInt64 {
		return nil, spool.ErrInvalid
	}
	limit := request.Limit
	if limit <= 0 {
		limit = spool.DefaultReplayLimit
	}
	if limit > spool.MaxReplayLimit {
		limit = spool.MaxReplayLimit
	}
	if err := ensureSpoolSchema(ctx, s.db); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, spoolMessageQuery+" WHERE message_id > ? ORDER BY message_id LIMIT ?", request.AfterMessageID, limit)
	if err != nil {
		return nil, fmt.Errorf("replay spool messages: %w", err)
	}
	defer rows.Close()
	values := make([]spool.Message, 0, limit)
	for rows.Next() {
		value, err := scanSpoolMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan replayed spool message: %w", err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spool replay: %w", err)
	}
	return values, nil
}

func (s *RuntimeStore) Acknowledge(ctx context.Context, request spool.AcknowledgeRequest) (spool.AcknowledgeResult, error) {
	if s == nil || s.db == nil || request.HighestContiguous == 0 || request.HighestContiguous > math.MaxInt64 {
		if request.HighestContiguous == 0 {
			return spool.AcknowledgeResult{}, nil
		}
		return spool.AcknowledgeResult{}, spool.ErrInvalid
	}
	if err := ensureSpoolSchema(ctx, s.db); err != nil {
		return spool.AcknowledgeResult{}, err
	}
	spoolWriteMu.Lock()
	defer spoolWriteMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return spool.AcknowledgeResult{}, fmt.Errorf("begin spool acknowledgement: %w", err)
	}
	defer tx.Rollback()
	if request.ReceiptID != "" {
		result, found, err := existingAcknowledgement(ctx, tx, request)
		if err != nil {
			return spool.AcknowledgeResult{}, err
		}
		if found {
			return result, nil
		}
	}
	state, err := loadSpoolState(ctx, tx)
	if err != nil {
		return spool.AcknowledgeResult{}, err
	}
	if request.HighestContiguous <= state.acknowledgedThrough {
		if request.ReceiptID != "" {
			if err := insertReceiptTx(ctx, tx, spool.Receipt{ID: request.ReceiptID, MessageID: request.HighestContiguous, PayloadHash: request.PayloadHash, Status: "acknowledged", ReceivedAt: request.ReceivedAt}); err != nil {
				return spool.AcknowledgeResult{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			return spool.AcknowledgeResult{}, fmt.Errorf("commit repeated spool acknowledgement: %w", err)
		}
		return spool.AcknowledgeResult{HighestContiguous: state.acknowledgedThrough, Acknowledged: state.acknowledgedThrough}, nil
	}
	if request.HighestContiguous >= state.nextMessageID {
		return spool.AcknowledgeResult{}, spool.ErrAckGap
	}
	if request.HighestContiguous > state.acknowledgedThrough {
		expected := int64(request.HighestContiguous - state.acknowledgedThrough)
		var count, bytes int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0) FROM spool_messages WHERE message_id > ? AND message_id <= ?`, state.acknowledgedThrough, request.HighestContiguous).Scan(&count, &bytes); err != nil {
			return spool.AcknowledgeResult{}, fmt.Errorf("inspect spool acknowledgement gap: %w", err)
		}
		if count != expected {
			return spool.AcknowledgeResult{}, spool.ErrAckGap
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM spool_messages WHERE message_id > ? AND message_id <= ?`, state.acknowledgedThrough, request.HighestContiguous); err != nil {
			return spool.AcknowledgeResult{}, fmt.Errorf("delete acknowledged spool messages: %w", err)
		}
		state.acknowledgedThrough = request.HighestContiguous
		state.bytesUsed -= bytes
		if state.bytesUsed < 0 {
			state.bytesUsed = 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE spool_state SET acknowledged_through = ?, bytes_used = ? WHERE id = 1`, state.acknowledgedThrough, state.bytesUsed); err != nil {
			return spool.AcknowledgeResult{}, fmt.Errorf("advance spool acknowledgement: %w", err)
		}
	}
	if request.ReceiptID != "" {
		if err := insertReceiptTx(ctx, tx, spool.Receipt{ID: request.ReceiptID, MessageID: request.HighestContiguous, PayloadHash: request.PayloadHash, Status: "acknowledged", ReceivedAt: request.ReceivedAt}); err != nil {
			return spool.AcknowledgeResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return spool.AcknowledgeResult{}, fmt.Errorf("commit spool acknowledgement: %w", err)
	}
	return spool.AcknowledgeResult{HighestContiguous: state.acknowledgedThrough, Acknowledged: state.acknowledgedThrough}, nil
}

func existingAcknowledgement(ctx context.Context, tx *sql.Tx, request spool.AcknowledgeRequest) (spool.AcknowledgeResult, bool, error) {
	var messageID int64
	var hash, status string
	if err := tx.QueryRowContext(ctx, `SELECT message_id, payload_hash, status FROM spool_receipts WHERE receipt_id = ?`, request.ReceiptID).Scan(&messageID, &hash, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spool.AcknowledgeResult{}, false, nil
		}
		return spool.AcknowledgeResult{}, false, fmt.Errorf("load spool acknowledgement receipt: %w", err)
	}
	if messageID != int64(request.HighestContiguous) || (request.PayloadHash != "" && hash != request.PayloadHash) || status != "acknowledged" {
		return spool.AcknowledgeResult{}, false, spool.ErrReceiptConflict
	}
	return spool.AcknowledgeResult{HighestContiguous: request.HighestContiguous, Acknowledged: request.HighestContiguous, Duplicate: true}, true, nil
}

func (s *RuntimeStore) RecordReceipt(ctx context.Context, receipt spool.Receipt) (spool.ReceiptResult, error) {
	if s == nil || s.db == nil || strings.TrimSpace(receipt.ID) == "" || receipt.MessageID == 0 || receipt.MessageID > math.MaxInt64 {
		return spool.ReceiptResult{}, spool.ErrInvalid
	}
	receipt.ID = strings.TrimSpace(receipt.ID)
	if receipt.Status == "" {
		receipt.Status = "received"
	}
	if receipt.ReceivedAt.IsZero() {
		receipt.ReceivedAt = time.Now().UTC()
	}
	receipt.ReceivedAt = receipt.ReceivedAt.UTC()
	if err := ensureSpoolSchema(ctx, s.db); err != nil {
		return spool.ReceiptResult{}, err
	}
	spoolWriteMu.Lock()
	defer spoolWriteMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return spool.ReceiptResult{}, fmt.Errorf("begin spool receipt: %w", err)
	}
	defer tx.Rollback()
	var messageID int64
	var hash, status, received string
	err = tx.QueryRowContext(ctx, `SELECT message_id, payload_hash, status, received_at FROM spool_receipts WHERE receipt_id = ?`, receipt.ID).Scan(&messageID, &hash, &status, &received)
	if err == nil {
		if messageID != int64(receipt.MessageID) || hash != receipt.PayloadHash || status != receipt.Status {
			return spool.ReceiptResult{}, spool.ErrReceiptConflict
		}
		parsed, parseErr := parseTimestamp(received)
		if parseErr != nil {
			return spool.ReceiptResult{}, parseErr
		}
		receipt.ReceivedAt = parsed
		return spool.ReceiptResult{Receipt: receipt, Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spool.ReceiptResult{}, fmt.Errorf("load spool receipt: %w", err)
	}
	if err := insertReceiptTx(ctx, tx, receipt); err != nil {
		return spool.ReceiptResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return spool.ReceiptResult{}, fmt.Errorf("commit spool receipt: %w", err)
	}
	return spool.ReceiptResult{Receipt: receipt}, nil
}

func insertReceiptTx(ctx context.Context, tx *sql.Tx, receipt spool.Receipt) error {
	if receipt.ReceivedAt.IsZero() {
		receipt.ReceivedAt = time.Now().UTC()
	}
	if receipt.Status == "" {
		receipt.Status = "received"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO spool_receipts (receipt_id, message_id, payload_hash, status, received_at) VALUES (?, ?, ?, ?, ?)`, receipt.ID, receipt.MessageID, receipt.PayloadHash, receipt.Status, timestamp(receipt.ReceivedAt.UTC()))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "spool_receipts") && strings.Contains(strings.ToLower(err.Error()), "unique") {
			return spool.ErrReceiptConflict
		}
		return fmt.Errorf("insert spool receipt: %w", err)
	}
	return nil
}

func (s *RuntimeStore) Usage(ctx context.Context) (spool.Usage, error) {
	if s == nil || s.db == nil {
		return spool.Usage{}, spool.ErrInvalid
	}
	if err := ensureSpoolSchema(ctx, s.db); err != nil {
		return spool.Usage{}, err
	}
	state, err := loadSpoolState(ctx, s.db)
	if err != nil {
		return spool.Usage{}, err
	}
	return spool.Usage{BytesUsed: state.bytesUsed, AcknowledgedThrough: state.acknowledgedThrough}, nil
}

func ensureSpoolSchema(ctx context.Context, db *sql.DB) error {
	spoolSchemaMu.Lock()
	defer spoolSchemaMu.Unlock()
	if _, err := db.ExecContext(ctx, spoolSchema); err != nil {
		return fmt.Errorf("ensure spool schema: %w", err)
	}
	return nil
}

func loadSpoolState(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (spoolState, error) {
	var next, acknowledged, bytes int64
	if err := db.QueryRowContext(ctx, `SELECT next_message_id, acknowledged_through, bytes_used FROM spool_state WHERE id = 1`).Scan(&next, &acknowledged, &bytes); err != nil {
		return spoolState{}, fmt.Errorf("load spool state: %w", err)
	}
	if next <= 0 || acknowledged < 0 || bytes < 0 || next > math.MaxInt64 || acknowledged > math.MaxInt64 {
		return spoolState{}, spool.ErrCriticalReserve
	}
	return spoolState{nextMessageID: uint64(next), acknowledgedThrough: uint64(acknowledged), bytesUsed: bytes}, nil
}

const spoolMessageQuery = `SELECT message_id, execution_id, sequence, lane, event_type, provider_event_id, payload, payload_hash, size_bytes, created_at FROM spool_messages`

func scanSpoolMessage(row spoolScanner) (spool.Message, error) {
	var value spool.Message
	var messageID, sequence int64
	var payload []byte
	var created string
	if err := row.Scan(&messageID, &value.ExecutionID, &sequence, &value.Lane, &value.Type, &value.ProviderEventID, &payload, &value.PayloadHash, &value.SizeBytes, &created); err != nil {
		return spool.Message{}, err
	}
	if messageID <= 0 || sequence <= 0 || messageID > math.MaxInt64 || sequence > math.MaxInt64 {
		return spool.Message{}, spool.ErrCriticalReserve
	}
	value.MessageID = uint64(messageID)
	value.Sequence = uint64(sequence)
	value.Payload = append([]byte(nil), payload...)
	parsed, err := parseTimestamp(created)
	if err != nil {
		return spool.Message{}, err
	}
	value.CreatedAt = parsed
	return value, nil
}

func hashPayload(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func addBytes(left, right int64) (int64, bool) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, true
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, true
	}
	return left + right, false
}

var _ spool.Store = (*RuntimeStore)(nil)
