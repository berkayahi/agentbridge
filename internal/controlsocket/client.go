package controlsocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

type Client struct{ Path string }

func (c Client) Call(ctx context.Context, request Request, result any) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(encoded) > MaxMessageBytes {
		return ErrTooLarge
	}
	if deadline, ok := ctx.Deadline(); ok {
		request.DeadlineUnixNano = deadline.UnixNano()
		encoded, _ = json.Marshal(request)
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	stopCancelWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancelWatch()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4*1024), MaxMessageBytes)
	if !scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: no response", ErrUnavailable)
	}
	var response response
	if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
		return ErrInvalid
	}
	if response.Error != "" {
		return decodeError(response.Error)
	}
	if result != nil && len(response.Result) > 0 {
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("decode control response: %w", err)
		}
	}
	return nil
}

func decodeError(code string) error {
	switch code {
	case "unauthorized":
		return ErrUnauthorized
	case "too_large":
		return ErrTooLarge
	case "deadline_exceeded":
		return context.DeadlineExceeded
	case "canceled":
		return context.Canceled
	default:
		return errors.New("control request failed")
	}
}
