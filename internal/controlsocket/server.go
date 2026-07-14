// Package controlsocket provides the owner-only local daemon boundary.
package controlsocket

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const MaxMessageBytes = 64 * 1024

var (
	ErrUnauthorized = errors.New("control request unauthorized")
	ErrTooLarge     = errors.New("control request too large")
	ErrUnavailable  = errors.New("control daemon unavailable")
	ErrInvalid      = errors.New("invalid control request")
)

type Request struct {
	TaskID           string          `json:"task_id"`
	Provider         string          `json:"provider"`
	Capability       []byte          `json:"capability"`
	Tool             string          `json:"tool"`
	Params           json.RawMessage `json:"params,omitempty"`
	DeadlineUnixNano int64           `json:"deadline_unix_nano,omitempty"`
}

type Handler interface {
	Handle(context.Context, Request) (any, error)
}

type HandlerFunc func(context.Context, Request) (any, error)

func (f HandlerFunc) Handle(ctx context.Context, request Request) (any, error) {
	return f(ctx, request)
}

type grant struct {
	provider   string
	capability []byte
}

type response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Server struct {
	path    string
	handler Handler

	mu        sync.Mutex
	grants    map[string]grant
	listener  *net.UnixListener
	conns     map[*net.UnixConn]struct{}
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewServer(path string, handler Handler) *Server {
	return &Server{path: path, handler: handler, grants: make(map[string]grant), conns: make(map[*net.UnixConn]struct{}), closed: make(chan struct{})}
}

func (s *Server) Grant(taskID, provider string, capability []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[taskID] = grant{provider: provider, capability: append([]byte(nil), capability...)}
}

func (s *Server) Start() error {
	if s.path == "" || s.handler == nil {
		return ErrInvalid
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure runtime directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("runtime directory is not owner-only")
	}
	if existing, err := os.Lstat(s.path); err == nil {
		if existing.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("control path exists and is not a socket")
		}
		if err := os.Remove(s.path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen control socket: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		_ = listener.Close()
		return err
	}
	s.listener = listener
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close()
		}
		s.mu.Unlock()
	})
	s.wg.Wait()
	_ = os.Remove(s.path)
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				continue
			}
		}
		if err := validatePeerUID(conn, os.Getuid()); err != nil {
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn *net.UnixConn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4*1024), MaxMessageBytes)
	if !scanner.Scan() {
		if scanner.Err() != nil {
			s.writeResponse(conn, nil, ErrTooLarge)
		}
		return
	}
	var request Request
	if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
		s.writeResponse(conn, nil, ErrInvalid)
		return
	}
	if err := s.authorize(request); err != nil {
		s.writeResponse(conn, nil, err)
		return
	}
	ctx := context.Background()
	if request.DeadlineUnixNano > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.Unix(0, request.DeadlineUnixNano))
		defer cancel()
	}
	result, err := s.handler.Handle(ctx, request)
	s.writeResponse(conn, result, err)
}

func (s *Server) authorize(request Request) error {
	if request.TaskID == "" || request.Provider == "" || request.Tool == "" || len(request.Capability) == 0 {
		return ErrUnauthorized
	}
	s.mu.Lock()
	grant, ok := s.grants[request.TaskID]
	s.mu.Unlock()
	if !ok || grant.provider != request.Provider || len(grant.capability) != len(request.Capability) {
		return ErrUnauthorized
	}
	if subtle.ConstantTimeCompare(grant.capability, request.Capability) != 1 {
		return ErrUnauthorized
	}
	return nil
}

func (s *Server) writeResponse(conn net.Conn, result any, err error) {
	res := response{}
	if err != nil {
		res.Error = errorCode(err)
	} else if result != nil {
		res.Result, _ = json.Marshal(result)
	}
	_ = json.NewEncoder(conn).Encode(res)
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "request_failed"
	}
}
