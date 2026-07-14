package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
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

func TestClientMapsBoundedInlineKeyboardsOnSendAndEdit(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	keyboard := InlineKeyboard{{{Text: "Approve", CallbackData: "signed-approve"}, {Text: "Reject", CallbackData: "signed-reject"}}}
	ref, err := c.Send(context.Background(), Message{ChatID: 100, Text: "Decision required", InlineKeyboard: keyboard})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Edit(context.Background(), ref, Message{Text: "Decision recorded", InlineKeyboard: keyboard}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, request := range api.requests[:2] {
		markup, ok := request.Values["reply_markup"].(map[string]any)
		if !ok {
			t.Fatalf("%s reply_markup = %#v", request.Method, request.Values["reply_markup"])
		}
		rows, ok := markup["inline_keyboard"].([]any)
		if !ok || len(rows) != 1 {
			t.Fatalf("%s inline_keyboard = %#v", request.Method, markup["inline_keyboard"])
		}
	}
}

func TestClientRejectsUnboundedOrMalformedInlineKeyboards(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	tests := []InlineKeyboard{
		{{}},
		{{{Text: "", CallbackData: "data"}}},
		{{{Text: "Approve", CallbackData: strings.Repeat("x", 65)}}},
		{{{Text: "Approve", CallbackData: "data"}, {Text: "Reject", CallbackData: "data"}, {Text: "One", CallbackData: "data"}, {Text: "Two", CallbackData: "data"}, {Text: "Three", CallbackData: "data"}}},
	}
	for index, keyboard := range tests {
		if _, err := c.Send(context.Background(), Message{ChatID: 100, Text: "Decision", InlineKeyboard: keyboard}); err == nil {
			t.Errorf("keyboard %d accepted: %#v", index, keyboard)
		}
	}
	if methods := api.Methods(); len(methods) != 0 {
		t.Fatalf("invalid keyboards reached Telegram: %v", methods)
	}
}

func TestConvertMessageExposesLargestPhotoAndImageDocumentMetadata(t *testing.T) {
	now := time.Unix(2_000, 0).UTC()
	photo := convertMessage(&models.Message{
		ID: 7, Date: int(now.Unix()), Chat: models.Chat{ID: 100, Type: models.ChatTypePrivate}, Caption: "inspect",
		Photo: []models.PhotoSize{{FileID: "small", FileUniqueID: "u-small", Width: 100, Height: 100, FileSize: 2}, {FileID: "large", FileUniqueID: "u-large", Width: 800, Height: 600, FileSize: 20}},
	})
	if photo.ReceivedAt != now || photo.Attachment == nil {
		t.Fatalf("photo message = %#v", photo)
	}
	if got := *photo.Attachment; got.FileID != "large" || got.UniqueID != "u-large" || got.MediaType != "image/jpeg" || got.SizeBytes != 20 || got.Width != 800 || got.Height != 600 {
		t.Fatalf("photo metadata = %#v", got)
	}

	document := convertMessage(&models.Message{
		ID: 8, Date: int(now.Unix()), Chat: models.Chat{ID: 100, Type: models.ChatTypePrivate},
		Document: &models.Document{FileID: "doc", FileUniqueID: "u-doc", FileName: "screen.webp", MimeType: "image/webp", FileSize: 123},
	})
	if document.Attachment == nil {
		t.Fatal("document metadata missing")
	}
	if got := *document.Attachment; got.FileID != "doc" || got.UniqueID != "u-doc" || got.Filename != "screen.webp" || got.MediaType != "image/webp" || got.SizeBytes != 123 {
		t.Fatalf("document metadata = %#v", got)
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

func TestClientOpensTelegramFileThroughNarrowReader(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	api.handler = func(method string, w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/file/bot") {
			_, _ = w.Write([]byte("image-bytes"))
			return
		}
		if method != "getFile" {
			t.Fatalf("method=%s", method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "remote", "file_unique_id": "unique", "file_size": 11, "file_path": "images/safe.png"}})
	}
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), RetryAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := c.Open(context.Background(), "remote")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "image-bytes" {
		t.Fatalf("data=%q", data)
	}
}

func TestClientRunStopsWhenUpdateConsumerIsAbsent(t *testing.T) {
	api := newFakeBotAPI(t)
	defer api.Close()
	requested := make(chan struct{})
	var once sync.Once
	api.handler = func(method string, w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requested) })
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{incomingUpdateJSON(1, "one"), incomingUpdateJSON(2, "two"), incomingUpdateJSON(3, "three")}})
	}
	c, err := NewClient("123:test", ClientOptions{ServerURL: api.URL(), HTTPClient: api.server.Client(), PollTimeout: time.Second, ReplayCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not stop without a consumer")
	}
}

func incomingUpdateJSON(id int64, text string) map[string]any {
	return map[string]any{"update_id": id, "message": map[string]any{"message_id": id, "date": 1, "text": text, "from": map[string]any{"id": 42, "is_bot": false, "first_name": "x"}, "chat": map[string]any{"id": 100, "type": "private"}}}
}
