package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	defaultMaxPending = 128
	defaultMaxLine    = 1 << 20
	defaultQueueSize  = 128
)

var (
	ErrClosed          = errors.New("codex JSON-RPC client closed")
	ErrProcessExited   = errors.New("codex app server exited")
	ErrProtocol        = errors.New("codex JSON-RPC protocol error")
	ErrUnknownResponse = errors.New("unknown or duplicate JSON-RPC response")
	ErrTooManyPending  = errors.New("too many pending JSON-RPC calls")
)

type ClientOptions struct {
	MaxPending int
	MaxLine    int
}

type ServerMessage struct {
	ID     string
	RawID  json.RawMessage
	Method string
	Params json.RawMessage
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("codex JSON-RPC error %d: %s", e.Code, e.Message)
}

type response struct {
	result json.RawMessage
	err    error
}

type Client struct {
	reader       io.Reader
	writer       io.Writer
	readerCloser io.Closer
	writerCloser io.Closer

	writes        chan wireMessage
	notifications chan ServerMessage
	requests      chan ServerMessage
	errors        chan error
	done          chan struct{}

	maxPending int
	nextID     atomic.Uint64
	mu         sync.Mutex
	pending    map[string]chan response
	closeOnce  sync.Once
	wg         sync.WaitGroup
}

func NewClient(reader io.Reader, writer io.Writer, options ClientOptions) *Client {
	maxPending := options.MaxPending
	if maxPending <= 0 {
		maxPending = defaultMaxPending
	}
	maxLine := options.MaxLine
	if maxLine <= 0 {
		maxLine = defaultMaxLine
	}
	c := &Client{
		reader:        reader,
		writer:        writer,
		writes:        make(chan wireMessage, defaultQueueSize),
		notifications: make(chan ServerMessage, defaultQueueSize),
		requests:      make(chan ServerMessage, defaultQueueSize),
		errors:        make(chan error, 16),
		done:          make(chan struct{}),
		maxPending:    maxPending,
		pending:       make(map[string]chan response),
	}
	if closer, ok := reader.(io.Closer); ok {
		c.readerCloser = closer
	}
	if closer, ok := writer.(io.Closer); ok {
		c.writerCloser = closer
	}
	c.wg.Add(2)
	go c.writeLoop()
	go c.readLoop(maxLine)
	return c
}

func (c *Client) Notifications() <-chan ServerMessage { return c.notifications }
func (c *Client) Requests() <-chan ServerMessage      { return c.requests }
func (c *Client) Errors() <-chan error                { return c.errors }
func (c *Client) Done() <-chan struct{}               { return c.done }

func (c *Client) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	replies := make(chan response, 1)
	c.mu.Lock()
	if len(c.pending) >= c.maxPending {
		c.mu.Unlock()
		return ErrTooManyPending
	}
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.pending[id] = replies
	c.mu.Unlock()

	message := wireMessage{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: params}
	select {
	case c.writes <- message:
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case <-c.done:
		c.removePending(id)
		return ErrClosed
	}

	select {
	case reply := <-replies:
		if reply.err != nil {
			return reply.err
		}
		if result == nil || len(reply.result) == 0 || string(reply.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(reply.result, result); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case <-c.done:
		select {
		case reply := <-replies:
			return reply.err
		default:
			return ErrProcessExited
		}
	}
}

func (c *Client) Notify(ctx context.Context, method string, params any) error {
	message := wireMessage{JSONRPC: "2.0", Method: method, Params: params}
	select {
	case c.writes <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}

func (c *Client) Respond(ctx context.Context, id json.RawMessage, result any, responseErr *rpcError) error {
	if len(id) == 0 || !json.Valid(id) {
		return fmt.Errorf("%w: missing request id", ErrProtocol)
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return err
	}
	message := wireMessage{JSONRPC: "2.0", ID: append(json.RawMessage(nil), id...), Result: encodedResult, Error: responseErr}
	select {
	case c.writes <- message:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}

func (c *Client) RespondResult(ctx context.Context, id json.RawMessage, result any) error {
	return c.Respond(ctx, id, result, nil)
}

func (c *Client) Close() error {
	c.finish(ErrClosed)
	if c.writerCloser != nil {
		_ = c.writerCloser.Close()
	}
	if c.readerCloser != nil {
		_ = c.readerCloser.Close()
	}
	c.wg.Wait()
	return nil
}

func (c *Client) writeLoop() {
	defer c.wg.Done()
	encoder := json.NewEncoder(c.writer)
	for {
		select {
		case <-c.done:
			return
		case message := <-c.writes:
			if err := encoder.Encode(message); err != nil {
				c.report(fmt.Errorf("%w: write failed", ErrProtocol))
				c.finish(ErrProcessExited)
				return
			}
		}
	}
}

func (c *Client) readLoop(maxLine int) {
	defer c.wg.Done()
	scanner := bufio.NewScanner(c.reader)
	scanner.Buffer(make([]byte, 64*1024), maxLine)
	for scanner.Scan() {
		var message wireMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil || (message.JSONRPC != "" && message.JSONRPC != "2.0") {
			c.report(fmt.Errorf("%w: malformed message", ErrProtocol))
			continue
		}
		c.dispatch(message)
	}
	if err := scanner.Err(); err != nil {
		c.report(fmt.Errorf("%w: line limit or read failure", ErrProtocol))
	}
	c.finish(ErrProcessExited)
}

func (c *Client) dispatch(message wireMessage) {
	if len(message.ID) > 0 && message.Method == "" {
		id := normalizeID(message.ID)
		c.mu.Lock()
		replies, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if !ok {
			c.report(ErrUnknownResponse)
			return
		}
		if message.Error != nil {
			replies <- response{err: message.Error}
			return
		}
		replies <- response{result: message.Result}
		return
	}
	if message.Method == "" {
		c.report(fmt.Errorf("%w: missing method", ErrProtocol))
		return
	}
	serverMessage := ServerMessage{ID: normalizeID(message.ID), RawID: append(json.RawMessage(nil), message.ID...), Method: message.Method}
	if message.Params != nil {
		serverMessage.Params, _ = json.Marshal(message.Params)
	}
	if len(message.ID) > 0 {
		c.deliver(c.requests, serverMessage)
		return
	}
	c.deliver(c.notifications, serverMessage)
}

func (c *Client) deliver(ch chan ServerMessage, message ServerMessage) {
	select {
	case ch <- message:
	default:
		c.report(fmt.Errorf("%w: inbound queue full", ErrProtocol))
	}
}

func (c *Client) removePending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) finish(err error) {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		pending := c.pending
		c.pending = make(map[string]chan response)
		c.mu.Unlock()
		for _, replies := range pending {
			replies <- response{err: err}
		}
	})
}

func (c *Client) report(err error) {
	select {
	case c.errors <- err:
	default:
	}
}

func normalizeID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}
