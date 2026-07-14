package telegram

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/berkayahi/agentbridge/internal/task"
)

type TaskStatus struct {
	TaskID        string
	ChatID        int64
	State         task.State
	CurrentAction string
	StartedAt     time.Time
	RepoProfile   string
	DeliveryRef   string
	Important     bool
}

type projection struct {
	mu       sync.Mutex
	ref      MessageRef
	lastText string
	lastEdit time.Time
	pending  string
}

// StatusProjector is safe for concurrent task updates.
type StatusProjector struct {
	mu        sync.Mutex
	messenger Messenger
	interval  time.Duration
	now       func() time.Time
	tasks     map[string]*projection
}

func NewStatusProjector(messenger Messenger, interval time.Duration, now func() time.Time) *StatusProjector {
	if interval < 0 {
		interval = 0
	}
	if now == nil {
		now = time.Now
	}
	return &StatusProjector{messenger: messenger, interval: interval, now: now, tasks: make(map[string]*projection)}
}

func (p *StatusProjector) Project(ctx context.Context, status TaskStatus) error {
	if status.TaskID == "" || status.ChatID == 0 || !status.State.Valid() {
		return errors.New("telegram: invalid task status")
	}
	now := p.now()
	text := renderStatus(status, now)
	p.mu.Lock()
	current, ok := p.tasks[status.TaskID]
	if !ok {
		current = &projection{}
		p.tasks[status.TaskID] = current
	}
	p.mu.Unlock()
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.ref.MessageID == 0 {
		ref, err := p.messenger.Send(ctx, Message{ChatID: status.ChatID, Text: text})
		if err != nil {
			p.mu.Lock()
			if p.tasks[status.TaskID] == current {
				delete(p.tasks, status.TaskID)
			}
			p.mu.Unlock()
			return err
		}
		current.ref = ref
		current.lastText = text
		current.lastEdit = now
		return nil
	}
	if text == current.lastText || text == current.pending {
		return nil
	}
	if !status.Important && !urgentStatus(status.State) && now.Sub(current.lastEdit) < p.interval {
		current.pending = text
		return nil
	}
	if err := p.messenger.Edit(ctx, current.ref, Message{ChatID: status.ChatID, Text: text}); err != nil {
		return err
	}
	current.lastText = text
	current.lastEdit = now
	current.pending = ""
	return nil
}

func (p *StatusProjector) Flush(ctx context.Context, taskID string) error {
	p.mu.Lock()
	current, ok := p.tasks[taskID]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.pending == "" || current.pending == current.lastText {
		return nil
	}
	now := p.now()
	if now.Sub(current.lastEdit) < p.interval {
		return nil
	}
	if err := p.messenger.Edit(ctx, current.ref, Message{ChatID: current.ref.ChatID, Text: current.pending}); err != nil {
		return err
	}
	current.lastText = current.pending
	current.pending = ""
	current.lastEdit = now
	return nil
}

func urgentStatus(state task.State) bool {
	return state == task.AwaitingApproval || state == task.AwaitingAuth || state == task.Completed || state == task.Failed || state == task.Canceled || state == task.Paused
}

func renderStatus(status TaskStatus, now time.Time) string {
	elapsed := time.Duration(0)
	if !status.StartedAt.IsZero() && now.After(status.StartedAt) {
		elapsed = now.Sub(status.StartedAt)
	}
	lines := []string{fmt.Sprintf("Task: %s", status.TaskID), fmt.Sprintf("State: %s", status.State), fmt.Sprintf("Elapsed: %s", formatElapsed(elapsed)), fmt.Sprintf("Repository: %s", status.RepoProfile)}
	if status.CurrentAction != "" {
		lines = append(lines, "Action: "+status.CurrentAction)
	}
	if status.DeliveryRef != "" {
		lines = append(lines, "Delivery: "+status.DeliveryRef)
	}
	return strings.Join(lines, "\n")
}

func formatElapsed(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	seconds := int64(duration / time.Second)
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	remaining := seconds % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %02dm %02ds", hours, minutes, remaining)
	}
	return fmt.Sprintf("%dm %02ds", minutes, remaining)
}

type CallbackAction struct{ Action, TaskID, ApprovalID string }

type CallbackSigner struct {
	secret []byte
	now    func() time.Time
}

func NewCallbackSigner(secret []byte, now func() time.Time) *CallbackSigner {
	if now == nil {
		now = time.Now
	}
	return &CallbackSigner{secret: append([]byte(nil), secret...), now: now}
}

func (s *CallbackSigner) Sign(action CallbackAction, ttl time.Duration) (string, error) {
	if len(s.secret) < 16 || ttl <= 0 || !validCallbackPart(action.Action, 12) || !validCallbackPart(action.TaskID, 16) || !validOptionalCallbackPart(action.ApprovalID, 12) {
		return "", errors.New("telegram: invalid callback")
	}
	payload := strings.Join([]string{action.Action, action.TaskID, action.ApprovalID, strconv.FormatInt(s.now().Add(ttl).Unix(), 36)}, "|")
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signature := s.signature(encoded)
	token := "1." + encoded + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(token) > 64 {
		return "", errors.New("telegram: callback payload too large")
	}
	return token, nil
}

func (s *CallbackSigner) Verify(token string) (CallbackAction, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "1" {
		return CallbackAction{}, errors.New("telegram: invalid callback signature")
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || base64.RawURLEncoding.EncodeToString(provided) != parts[2] || !hmac.Equal(provided, s.signature(parts[1])) {
		return CallbackAction{}, errors.New("telegram: invalid callback signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return CallbackAction{}, errors.New("telegram: invalid callback payload")
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 4 {
		return CallbackAction{}, errors.New("telegram: invalid callback payload")
	}
	expires, err := strconv.ParseInt(fields[3], 36, 64)
	if err != nil || s.now().Unix() > expires {
		return CallbackAction{}, errors.New("telegram: callback expired")
	}
	action := CallbackAction{Action: fields[0], TaskID: fields[1], ApprovalID: fields[2]}
	if !validCallbackPart(action.Action, 12) || !validCallbackPart(action.TaskID, 16) || !validOptionalCallbackPart(action.ApprovalID, 12) {
		return CallbackAction{}, errors.New("telegram: invalid callback payload")
	}
	return action, nil
}

// ApprovalKeyboard creates a compact signed decision keyboard that fits
// Telegram's 64-byte callback_data bound.
func ApprovalKeyboard(signer *CallbackSigner, taskID, approvalID string, ttl time.Duration) (InlineKeyboard, error) {
	if signer == nil {
		return nil, errors.New("telegram: callback signer is required")
	}
	approve, err := signer.Sign(CallbackAction{Action: "approve", TaskID: taskID, ApprovalID: approvalID}, ttl)
	if err != nil {
		return nil, err
	}
	reject, err := signer.Sign(CallbackAction{Action: "reject", TaskID: taskID, ApprovalID: approvalID}, ttl)
	if err != nil {
		return nil, err
	}
	return InlineKeyboard{{{Text: "Approve", CallbackData: approve}, {Text: "Reject", CallbackData: reject}}}, nil
}

func SessionKeyboard(signer *CallbackSigner, provider task.Provider, tasks []task.Task, ttl time.Duration) (InlineKeyboard, error) {
	if signer == nil {
		return nil, errors.New("telegram: callback signer is required")
	}
	newTask, err := signer.Sign(CallbackAction{Action: "session_new", TaskID: string(provider)}, ttl)
	if err != nil {
		return nil, err
	}
	keyboard := InlineKeyboard{{{Text: "➕ New task", CallbackData: newTask}}}
	for _, value := range tasks {
		callback, signErr := signer.Sign(CallbackAction{Action: "session_use", TaskID: value.ID}, ttl)
		if signErr != nil {
			return nil, signErr
		}
		label := value.Title
		if label == "" {
			label = value.ID
		}
		label = fmt.Sprintf("%s [%s] · %s", label, value.State, value.ID[:min(len(value.ID), 6)])
		keyboard = append(keyboard, []InlineButton{{Text: label, CallbackData: callback}})
	}
	return keyboard, nil
}

func (s *CallbackSigner) signature(payload string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)[:10]
}
func validOptionalCallbackPart(value string, max int) bool {
	return value == "" || validCallbackPart(value, max)
}
func validCallbackPart(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
