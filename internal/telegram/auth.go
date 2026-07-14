package telegram

import (
	"errors"
	"sync"
	"time"
)

var ErrUnauthorized = errors.New("telegram: unauthorized update")

// Authorizer is safe for concurrent update handling.
type Authorizer struct {
	mu             sync.Mutex
	allowed        map[int64]struct{}
	pairedChatID   int64
	seen           map[int64]struct{}
	order          []int64
	replayCapacity int
	callbackMaxAge time.Duration
	now            func() time.Time
}

func NewAuthorizer(allowedUserIDs []int64, pairedChatID int64, replayCapacity int, callbackMaxAge time.Duration, now func() time.Time) *Authorizer {
	allowed := make(map[int64]struct{}, len(allowedUserIDs))
	for _, id := range allowedUserIDs {
		allowed[id] = struct{}{}
	}
	if replayCapacity < 1 {
		replayCapacity = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Authorizer{allowed: allowed, pairedChatID: pairedChatID, seen: make(map[int64]struct{}), replayCapacity: replayCapacity, callbackMaxAge: callbackMaxAge, now: now}
}

func (a *Authorizer) Authorize(update Update) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, duplicate := a.seen[update.ID]; duplicate {
		return ErrUnauthorized
	}
	a.remember(update.ID)
	user, chat, ok := update.identity()
	if !ok || chat.Type != ChatPrivate {
		return ErrUnauthorized
	}
	if _, ok := a.allowed[user.ID]; !ok {
		return ErrUnauthorized
	}
	if a.pairedChatID != 0 && chat.ID != a.pairedChatID {
		return ErrUnauthorized
	}
	if update.Callback != nil && a.callbackMaxAge > 0 {
		at := update.Callback.ReceivedAt
		if at.IsZero() || a.now().Sub(at) > a.callbackMaxAge || at.After(a.now().Add(time.Minute)) {
			return ErrUnauthorized
		}
	}
	return nil
}

func (a *Authorizer) remember(id int64) {
	a.seen[id] = struct{}{}
	a.order = append(a.order, id)
	if len(a.order) <= a.replayCapacity {
		return
	}
	delete(a.seen, a.order[0])
	a.order = a.order[1:]
}

func (a *Authorizer) ReplayEntries() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}
