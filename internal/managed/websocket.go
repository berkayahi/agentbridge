package managed

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const managedWebSocketProtocol = "agentbridge.managed.v1"

var (
	ErrInvalidWebSocket        = errors.New("managed: invalid websocket configuration")
	ErrInvalidWebSocketMessage = errors.New("managed: invalid websocket message")
)

type WebSocketConfig struct {
	URL             string
	Origin          string
	TLSConfig       *tls.Config
	Header          http.Header
	Dialer          *net.Dialer
	MaxMessageBytes int
	ReadPoll        time.Duration
}

// WebSocketTransport is the concrete outbound WSS transport for the managed
// client. It uses binary JSON frames at the transport boundary while all
// signatures remain over the transport-neutral Frame/Handshake canonicals.
type WebSocketTransport struct {
	conn      *websocket.Conn
	maxBytes  int
	readPoll  time.Duration
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func ValidateWebSocketURL(raw string) error {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return ErrInvalidWebSocket
	}
	return nil
}

func NewWebSocketTransport(ctx context.Context, config WebSocketConfig) (*WebSocketTransport, error) {
	if err := ValidateWebSocketURL(config.URL); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil {
		return nil, ErrInvalidWebSocket
	}
	tlsConfig := config.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		tlsConfig = tlsConfig.Clone()
		if tlsConfig.InsecureSkipVerify {
			return nil, ErrInvalidWebSocket
		}
		if tlsConfig.MinVersion == 0 {
			tlsConfig.MinVersion = tls.VersionTLS12
		}
	}
	origin := strings.TrimSpace(config.Origin)
	if origin == "" {
		origin = "https://" + parsed.Host
	}
	originURL, err := url.Parse(origin)
	if err != nil || originURL.Scheme != "https" || originURL.Host == "" || originURL.User != nil {
		return nil, ErrInvalidWebSocket
	}
	wsConfig := &websocket.Config{
		Location: parsed, Origin: originURL, Protocol: []string{managedWebSocketProtocol},
		Version: websocket.ProtocolVersionHybi13, TlsConfig: tlsConfig,
		Header: config.Header.Clone(), Dialer: config.Dialer,
	}
	conn, err := wsConfig.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial managed gateway: %w", err)
	}
	return NewWebSocketTransportFromConn(conn, config.MaxMessageBytes, config.ReadPoll), nil
}

func NewWebSocketTransportFromConn(conn *websocket.Conn, maxMessageBytes int, readPoll time.Duration) *WebSocketTransport {
	if maxMessageBytes <= 0 || maxMessageBytes > MaxFrameBytes {
		maxMessageBytes = MaxFrameBytes
	}
	if readPoll <= 0 {
		readPoll = time.Second
	}
	if conn != nil {
		conn.MaxPayloadBytes = maxMessageBytes
	}
	return &WebSocketTransport{conn: conn, maxBytes: maxMessageBytes, readPoll: readPoll}
}

func (t *WebSocketTransport) PerformHandshake(ctx context.Context, local Handshake) (Handshake, error) {
	if t == nil || t.conn == nil {
		return Handshake{}, ErrTransportClosed
	}
	if err := validateHandshake(local); err != nil {
		return Handshake{}, err
	}
	if err := t.writeEnvelope(ctx, webSocketEnvelope{Type: "handshake", Handshake: &local}); err != nil {
		return Handshake{}, err
	}
	envelope, err := t.readEnvelope(ctx)
	if err != nil {
		return Handshake{}, err
	}
	if envelope.Type != "handshake" || envelope.Handshake == nil {
		return Handshake{}, ErrInvalidWebSocketMessage
	}
	if err := validateHandshake(*envelope.Handshake); err != nil {
		return Handshake{}, err
	}
	return *envelope.Handshake, nil
}

func (t *WebSocketTransport) Receive(ctx context.Context) (Frame, error) {
	if t == nil || t.conn == nil {
		return Frame{}, ErrTransportClosed
	}
	envelope, err := t.readEnvelope(ctx)
	if err != nil {
		return Frame{}, err
	}
	if envelope.Type != "frame" || envelope.Frame == nil {
		return Frame{}, ErrInvalidWebSocketMessage
	}
	return *envelope.Frame, nil
}

func (t *WebSocketTransport) Send(ctx context.Context, frame Frame) error {
	if t == nil || t.conn == nil {
		return ErrTransportClosed
	}
	return t.writeEnvelope(ctx, webSocketEnvelope{Type: "frame", Frame: &frame})
}

func (t *WebSocketTransport) Close() error {
	if t == nil || t.conn == nil {
		return nil
	}
	t.closeOnce.Do(func() { t.closeErr = t.conn.Close() })
	return t.closeErr
}

type webSocketEnvelope struct {
	Type      string     `json:"type"`
	Handshake *Handshake `json:"handshake,omitempty"`
	Frame     *Frame     `json:"frame,omitempty"`
}

func marshalWebSocketEnvelope(envelope webSocketEnvelope) ([]byte, error) {
	if envelope.Type != "handshake" && envelope.Type != "frame" {
		return nil, ErrInvalidWebSocketMessage
	}
	if envelope.Type == "handshake" && (envelope.Handshake == nil || envelope.Frame != nil) {
		return nil, ErrInvalidWebSocketMessage
	}
	if envelope.Type == "frame" && (envelope.Frame == nil || envelope.Handshake != nil) {
		return nil, ErrInvalidWebSocketMessage
	}
	if envelope.Frame != nil && len(envelope.Frame.Payload) > MaxPayloadBytes {
		return nil, ErrInvalidFramePayload
	}
	value, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode websocket message: %w", err)
	}
	if len(value) > MaxFrameBytes {
		return nil, ErrInvalidFramePayload
	}
	return value, nil
}

func unmarshalWebSocketEnvelope(value []byte) (webSocketEnvelope, error) {
	if len(value) == 0 || len(value) > MaxFrameBytes {
		return webSocketEnvelope{}, ErrInvalidWebSocketMessage
	}
	var envelope webSocketEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return webSocketEnvelope{}, ErrInvalidWebSocketMessage
	}
	if envelope.Type == "handshake" && envelope.Handshake != nil && envelope.Frame == nil {
		return envelope, nil
	}
	if envelope.Type == "frame" && envelope.Frame != nil && envelope.Handshake == nil {
		return envelope, nil
	}
	return webSocketEnvelope{}, ErrInvalidWebSocketMessage
}

func (t *WebSocketTransport) writeEnvelope(ctx context.Context, envelope webSocketEnvelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := marshalWebSocketEnvelope(envelope)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := websocket.Message.Send(t.conn, value); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func (t *WebSocketTransport) readEnvelope(ctx context.Context) (webSocketEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return webSocketEnvelope{}, err
	}
	for {
		deadline := time.Now().Add(t.readPoll)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		if err := t.conn.SetReadDeadline(deadline); err != nil {
			return webSocketEnvelope{}, err
		}
		var value []byte
		err := websocket.Message.Receive(t.conn, &value)
		if err != nil {
			if ctx.Err() != nil {
				return webSocketEnvelope{}, ctx.Err()
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return webSocketEnvelope{}, err
		}
		if len(value) > t.maxBytes {
			return webSocketEnvelope{}, ErrInvalidWebSocketMessage
		}
		return unmarshalWebSocketEnvelope(value)
	}
}
