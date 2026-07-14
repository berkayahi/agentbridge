package telegram

import (
	"testing"
	"time"
)

func TestAuthorizerRequiresPrivateAllowedNumericIdentityAndPairedChat(t *testing.T) {
	now := time.Unix(2_000, 0)
	a := NewAuthorizer([]int64{42}, 100, 8, time.Minute, func() time.Time { return now })
	valid := Update{ID: 1, Message: &IncomingMessage{Chat: Chat{ID: 100, Type: ChatPrivate}, From: User{ID: 42}}}
	if err := a.Authorize(valid); err != nil {
		t.Fatalf("valid update: %v", err)
	}

	tests := []Update{
		{ID: 2, Message: &IncomingMessage{Chat: Chat{ID: 100, Type: ChatGroup}, From: User{ID: 42}}},
		{ID: 3, Message: &IncomingMessage{Chat: Chat{ID: 100, Type: ChatSupergroup}, From: User{ID: 42}}},
		{ID: 4, Message: &IncomingMessage{Chat: Chat{ID: 100, Type: ChatChannel}, From: User{ID: 42}}},
		{ID: 5, Message: &IncomingMessage{Chat: Chat{ID: 100, Type: ChatPrivate}, From: User{ID: 7, Username: "trusted"}}},
		{ID: 6, Message: &IncomingMessage{Chat: Chat{ID: 101, Type: ChatPrivate}, From: User{ID: 42}}},
	}
	for _, update := range tests {
		if err := a.Authorize(update); err == nil {
			t.Errorf("update %#v authorized", update)
		}
	}
}

func TestAuthorizerRejectsDuplicateAndStaleCallbacksWithBoundedReplayWindow(t *testing.T) {
	now := time.Unix(2_000, 0)
	a := NewAuthorizer([]int64{42}, 100, 2, time.Minute, func() time.Time { return now })
	message := &IncomingMessage{Chat: Chat{ID: 100, Type: ChatPrivate}, From: User{ID: 42}}
	if err := a.Authorize(Update{ID: 1, Message: message}); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(Update{ID: 1, Message: message}); err == nil {
		t.Fatal("duplicate authorized")
	}
	stale := Update{ID: 2, Callback: &CallbackQuery{From: User{ID: 42}, Message: *message, ReceivedAt: now.Add(-2 * time.Minute)}}
	if err := a.Authorize(stale); err == nil {
		t.Fatal("stale callback authorized")
	}
	if err := a.Authorize(Update{ID: 3, Message: message}); err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(Update{ID: 4, Message: message}); err != nil {
		t.Fatal(err)
	}
	if got := a.ReplayEntries(); got != 2 {
		t.Fatalf("replay entries = %d", got)
	}
}
