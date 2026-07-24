package localcontrol

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/berkayahi/agentbridge/internal/controlsocket"
)

// UnixServer owns an authenticated local HTTP API listener. Filesystem
// permissions and peer credentials are enforced before HTTP authentication.
type UnixServer struct {
	path     string
	listener *ownerListener
	server   *http.Server

	closeOnce   sync.Once
	cleanupOnce sync.Once
	mu          sync.Mutex
	closeErr    error
}

// ListenUnix binds an owner-only Unix socket and returns a ready server. The
// caller must run Serve and later Close it.
func ListenUnix(path string, handler http.Handler) (*UnixServer, error) {
	if handler == nil || strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return nil, ErrInvalidRequest
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create local API directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("secure local API directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("local API path is not a socket: %w", ErrInvalidRequest)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("replace local API socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect local API socket: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen local API socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("secure local API socket: %w", err)
	}
	owner := &ownerListener{listener: listener}
	return &UnixServer{path: path, listener: owner, server: &http.Server{Handler: handler}}, nil
}

func (s *UnixServer) Serve() error {
	if s == nil || s.server == nil || s.listener == nil {
		return ErrInvalidRequest
	}
	err := s.server.Serve(s.listener)
	s.cleanup()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *UnixServer) Close(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		err := s.server.Shutdown(ctx)
		if err != nil {
			_ = s.server.Close()
		}
		s.mu.Lock()
		s.closeErr = err
		s.mu.Unlock()
		s.cleanup()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

func (s *UnixServer) cleanup() {
	s.cleanupOnce.Do(func() {
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.path)
	})
}

// ServeUnix runs the authenticated local HTTP API on an owner-only Unix
// socket. The HTTP shared secret remains mandatory even after peer UID
// validation so a compromised same-user process cannot use ambient socket
// access as the only authorization factor.
func ServeUnix(ctx context.Context, path string, handler http.Handler) error {
	server, err := ListenUnix(path, handler)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = server.Close(context.Background())
		return <-done
	}
}

type ownerListener struct{ listener *net.UnixListener }

func (l *ownerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.listener.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if err := controlsocket.ValidateOwner(conn); err != nil {
			_ = conn.Close()
			continue
		}
		return conn, nil
	}
}

func (l *ownerListener) Close() error { return l.listener.Close() }

func (l *ownerListener) Addr() net.Addr { return l.listener.Addr() }
