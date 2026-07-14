package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	telebot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const maxTelegramError = 512

type Message struct {
	ChatID int64
	Text   string
}

type MessageRef struct {
	ChatID    int64
	MessageID int64
}

type Document struct {
	ChatID   int64
	Filename string
	Caption  string
	Data     io.Reader
}

type Messenger interface {
	Send(context.Context, Message) (MessageRef, error)
	Edit(context.Context, MessageRef, Message) error
	AnswerCallback(context.Context, string, string) error
	SendDocument(context.Context, Document) error
}

type ClientOptions struct {
	ServerURL      string
	HTTPClient     telebot.HttpClient
	RetryAttempts  int
	PollTimeout    time.Duration
	ReplayCapacity int
	Sleep          func(context.Context, time.Duration) error
	Jitter         func(time.Duration) time.Duration
	Now            func() time.Time
}

// Client is the only package type that depends on Telegram SDK types.
type Client struct {
	bot      *telebot.Bot
	attempts int
	sleep    func(context.Context, time.Duration) error
	jitter   func(time.Duration) time.Duration
	raw      <-chan Update

	mu       sync.Mutex
	running  bool
	seen     map[int64]struct{}
	order    []int64
	capacity int
}

func NewClient(token string, opts ClientOptions) (*Client, error) {
	if opts.RetryAttempts < 1 {
		opts.RetryAttempts = 3
	}
	if opts.PollTimeout <= time.Second {
		opts.PollTimeout = 2 * time.Second
	}
	if opts.ReplayCapacity < 1 {
		opts.ReplayCapacity = 128
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	if opts.Jitter == nil {
		opts.Jitter = func(time.Duration) time.Duration { return 0 }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	raw := make(chan Update, 1)
	options := []telebot.Option{
		telebot.WithSkipGetMe(),
		telebot.WithNotAsyncHandlers(),
		telebot.WithUpdatesChannelCap(1),
		telebot.WithInitialOffset(0),
		telebot.WithDefaultHandler(func(_ context.Context, _ *telebot.Bot, update *models.Update) {
			if converted, ok := convertUpdate(update, opts.Now()); ok {
				raw <- converted
			}
		}),
	}
	if opts.ServerURL != "" {
		options = append(options, telebot.WithServerURL(strings.TrimRight(opts.ServerURL, "/")))
	}
	if opts.HTTPClient != nil {
		options = append(options, telebot.WithHTTPClient(opts.PollTimeout, opts.HTTPClient))
	}
	b, err := telebot.New(token, options...)
	if err != nil {
		return nil, fmt.Errorf("create Telegram client: %w", err)
	}
	return &Client{bot: b, attempts: opts.RetryAttempts, sleep: opts.Sleep, jitter: opts.Jitter, raw: raw, seen: make(map[int64]struct{}), capacity: opts.ReplayCapacity}, nil
}

func (c *Client) Send(ctx context.Context, message Message) (MessageRef, error) {
	if message.ChatID == 0 || message.Text == "" {
		return MessageRef{}, errors.New("telegram: invalid message")
	}
	var sent *models.Message
	err := c.retry(ctx, func() error {
		var err error
		sent, err = c.bot.SendMessage(ctx, &telebot.SendMessageParams{ChatID: message.ChatID, Text: html.EscapeString(message.Text), ParseMode: models.ParseModeHTML})
		return err
	})
	if err != nil {
		return MessageRef{}, err
	}
	return MessageRef{ChatID: sent.Chat.ID, MessageID: int64(sent.ID)}, nil
}

func (c *Client) Edit(ctx context.Context, ref MessageRef, message Message) error {
	if ref.ChatID == 0 || ref.MessageID == 0 || message.Text == "" {
		return errors.New("telegram: invalid edit")
	}
	return c.retry(ctx, func() error {
		_, err := c.bot.EditMessageText(ctx, &telebot.EditMessageTextParams{ChatID: ref.ChatID, MessageID: int(ref.MessageID), Text: html.EscapeString(message.Text), ParseMode: models.ParseModeHTML})
		return err
	})
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID, text string) error {
	if callbackID == "" {
		return errors.New("telegram: callback ID is required")
	}
	return c.retry(ctx, func() error {
		_, err := c.bot.AnswerCallbackQuery(ctx, &telebot.AnswerCallbackQueryParams{CallbackQueryID: callbackID, Text: text})
		return err
	})
}

func (c *Client) SendDocument(ctx context.Context, document Document) error {
	if document.ChatID == 0 || document.Data == nil || document.Filename == "" {
		return errors.New("telegram: invalid document")
	}
	return c.retry(ctx, func() error {
		_, err := c.bot.SendDocument(ctx, &telebot.SendDocumentParams{ChatID: document.ChatID, Caption: html.EscapeString(document.Caption), ParseMode: models.ParseModeHTML,
			Document: &models.InputFileUpload{Filename: document.Filename, Data: document.Data}})
		return err
	})
}

func (c *Client) retry(ctx context.Context, operation func() error) error {
	var last error
	for attempt := 0; attempt < c.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = operation()
		if last == nil {
			return nil
		}
		if attempt == c.attempts-1 {
			break
		}
		delay := time.Duration(1<<attempt) * 100 * time.Millisecond
		var rateLimit *telebot.TooManyRequestsError
		if errors.As(last, &rateLimit) && rateLimit.RetryAfter > 0 {
			delay = time.Duration(rateLimit.RetryAfter) * time.Second
		}
		if err := c.sleep(ctx, delay+c.jitter(delay)); err != nil {
			return err
		}
	}
	message := last.Error()
	if len(message) > maxTelegramError {
		message = message[:maxTelegramError]
	}
	return fmt.Errorf("telegram request failed: %s", message)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run owns the SDK polling goroutines and blocks until ctx is canceled.
func (c *Client) Run(ctx context.Context) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	c.bot.Start(ctx)
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()
}

func (c *Client) Next(ctx context.Context) (Update, error) {
	for {
		select {
		case <-ctx.Done():
			return Update{}, ctx.Err()
		case update := <-c.raw:
			c.mu.Lock()
			_, duplicate := c.seen[update.ID]
			if !duplicate {
				c.seen[update.ID] = struct{}{}
				c.order = append(c.order, update.ID)
				if len(c.order) > c.capacity {
					delete(c.seen, c.order[0])
					c.order = c.order[1:]
				}
			}
			c.mu.Unlock()
			if !duplicate {
				return update, nil
			}
		}
	}
}

func convertUpdate(source *models.Update, now time.Time) (Update, bool) {
	if source == nil {
		return Update{}, false
	}
	result := Update{ID: source.ID}
	if source.Message != nil {
		result.Message = convertMessage(source.Message)
		return result, true
	}
	if source.CallbackQuery != nil && source.CallbackQuery.Message.Message != nil {
		result.Callback = &CallbackQuery{ID: source.CallbackQuery.ID, From: convertUser(source.CallbackQuery.From), Message: *convertMessage(source.CallbackQuery.Message.Message), Data: source.CallbackQuery.Data, ReceivedAt: now}
		return result, true
	}
	return Update{}, false
}

func convertMessage(source *models.Message) *IncomingMessage {
	message := &IncomingMessage{ID: int64(source.ID), Chat: Chat{ID: source.Chat.ID, Type: ChatType(source.Chat.Type)}, Text: source.Text, Caption: source.Caption, MediaGroupID: source.MediaGroupID}
	if source.From != nil {
		message.From = convertUser(*source.From)
	}
	if source.ReplyToMessage != nil {
		message.ReplyToMessageID = int64(source.ReplyToMessage.ID)
	}
	return message
}

func convertUser(source models.User) User { return User{ID: source.ID, Username: source.Username} }

var _ Messenger = (*Client)(nil)
var _ UpdateSource = (*Client)(nil)
var _ telebot.HttpClient = (*http.Client)(nil)
