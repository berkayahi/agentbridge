package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientSendsEscapedMessagesEditsCallbacksAndDocuments(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := c.Send(context.Background(), Message{ChatID: 100, Text: `<unsafe & text>`})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ChatID != 100 || ref.MessageID != 77 {
		t.Fatalf("ref = %#v", ref)
	}
	if err := c.Edit(context.Background(), ref, Message{Text: "updated"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AnswerCallback(context.Background(), "callback-1", "done"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendDocument(context.Background(), Document{ChatID: 100, Filename: "report.txt", Caption: "safe", Data: strings.NewReader("body")}); err != nil {
		t.Fatal(err)
	}
	methods := strings.Join(api.Methods(), ",")
	for _, want := range []string{"sendMessage", "editMessageText", "answerCallbackQuery", "sendDocument"} {
		if !strings.Contains(methods, want) {
			t.Errorf("methods %q missing %q", methods, want)
		}
	}
	api.mu.Lock()
	sent := api.requests[0].Values["text"]
	api.mu.Unlock()
	if sent != "&lt;unsafe &amp; text&gt;" {
		t.Fatalf("sent text = %#v", sent)
	}
}

func TestClientRetries429UsingRetryAfterAndInjectedJitter(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	var mu sync.Mutex
	attempts := 0
	var delays []time.Duration
	api.handler = func(method string, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current == 1 {
			writeAPIError(w, http.StatusTooManyRequests, "slow down", 2)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1, "date": 1, "chat": map[string]any{"id": 100, "type": "private"}}})
	}
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 2,
		Sleep: func(ctx context.Context, d time.Duration) error { delays = append(delays, d); return nil }, Jitter: func(time.Duration) time.Duration { return 25 * time.Millisecond }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(context.Background(), Message{ChatID: 100, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(delays) != 1 || delays[0] != 2*time.Second+25*time.Millisecond {
		t.Fatalf("attempts=%d delays=%v", attempts, delays)
	}
}

func TestClientCancellationStopsRetry(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	api.handler = func(method string, w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, 500, strings.Repeat("x", 10_000), 0)
	}
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 3,
		Sleep: func(ctx context.Context, d time.Duration) error { <-ctx.Done(); return ctx.Err() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Send(ctx, Message{ChatID: 100, Text: "hello"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if len(err.Error()) > 600 {
		t.Fatalf("unbounded error length = %d", len(err.Error()))
	}
}

func TestClientLongPollAdvancesOffsetAndSuppressesDuplicates(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	var mu sync.Mutex
	var offsets []float64
	calls := 0
	secondPoll := make(chan struct{})
	var secondPollOnce sync.Once
	api.handler = func(method string, w http.ResponseWriter, r *http.Request) {
		if method != "getUpdates" {
			t.Fatalf("method = %s", method)
		}
		body := decodeBotRequest(r)
		mu.Lock()
		offsets = append(offsets, body["offset"].(float64))
		calls++
		n := calls
		if n >= 2 {
			secondPollOnce.Do(func() { close(secondPoll) })
		}
		mu.Unlock()
		updates := []any{}
		if n == 1 {
			updates = []any{incomingUpdateJSON(5, "first"), incomingUpdateJSON(5, "duplicate"), incomingUpdateJSON(6, "second")}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": updates})
	}
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 1, PollTimeout: time.Second, ReplayCapacity: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	u1, err := c.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := c.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if u1.ID != 5 || u2.ID != 6 {
		t.Fatalf("IDs = %d, %d", u1.ID, u2.ID)
	}
	select {
	case <-secondPoll:
	case <-time.After(2 * time.Second):
		t.Fatal("second long poll did not start")
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(offsets) < 2 || offsets[0] != 1 || offsets[1] < 7 {
		t.Fatalf("offsets = %v", offsets)
	}
}

func incomingUpdateJSON(id int64, text string) map[string]any {
	return map[string]any{"update_id": id, "message": map[string]any{"message_id": id, "date": 1, "text": text, "from": map[string]any{"id": 42, "is_bot": false, "first_name": "x"}, "chat": map[string]any{"id": 100, "type": "private"}}}
}
