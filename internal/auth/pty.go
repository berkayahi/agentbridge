package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/berkayahi/agentbridge/internal/task"
	"github.com/creack/pty"
)

var (
	recoveryURLPattern  = regexp.MustCompile(`https?://[^\s<>"']+`)
	recoveryCodePattern = regexp.MustCompile(`\b[A-Z0-9]{4,}(?:-[A-Z0-9]{4,})+\b`)
)

// ExecCommandRunner executes non-interactive provider health commands. The
// caller supplies deadlines through ctx.
type ExecCommandRunner struct {
	Executables map[string]string
	Environment []string
}

func (r ExecCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, configuredExecutable(r.Executables, name), args...)
	if r.Environment != nil {
		cmd.Env = append([]string(nil), r.Environment...)
	}
	output, err := cmd.CombinedOutput()
	if err != nil && isExecutableMissing(err) {
		return output, fmt.Errorf("%w: %v", ErrCommandMissing, err)
	}
	return output, err
}

// ExecPTY runs one interactive login process synchronously. Closing the PTY on
// cancellation unblocks reads; CommandContext terminates and Wait reaps the
// provider child before Run returns.
type ExecPTY struct {
	Executables map[string]string
	Environment []string
}

func (r ExecPTY) Run(ctx context.Context, name string, args []string, input <-chan []byte, output func([]byte)) error {
	if output == nil {
		return errors.New("auth pty: output callback is required")
	}
	cmd := exec.CommandContext(ctx, configuredExecutable(r.Executables, name), args...)
	if r.Environment != nil {
		cmd.Env = append([]string(nil), r.Environment...)
	}
	terminal, err := pty.Start(cmd)
	if err != nil {
		if isExecutableMissing(err) {
			return fmt.Errorf("%w: %v", ErrCommandMissing, err)
		}
		return fmt.Errorf("start authentication pty: %w", err)
	}

	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	writerError := make(chan error, 1)
	go func() {
		defer close(watcherDone)
		for {
			select {
			case <-ctx.Done():
				terminateProcessGroup(cmd.Process.Pid)
				_ = terminal.Close()
				return
			case value := <-input:
				if len(value) == 0 {
					continue
				}
				_, writeErr := terminal.Write(value)
				clear(value)
				if writeErr != nil {
					writerError <- fmt.Errorf("write authentication pty: %w", writeErr)
					_ = terminal.Close()
					return
				}
			case <-stopWatcher:
				return
			}
		}
	}()

	reader := bufio.NewReader(terminal)
	for {
		line, readErr := reader.ReadSlice('\n')
		if len(line) > 0 {
			output(line)
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if readErr != nil {
			if !ignorablePTYReadError(readErr) && ctx.Err() == nil {
				err = fmt.Errorf("read authentication pty: %w", readErr)
			}
			break
		}
	}
	_ = terminal.Close()
	waitErr := cmd.Wait()
	close(stopWatcher)
	<-watcherDone
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case writeErr := <-writerError:
		return writeErr
	default:
	}
	if err != nil {
		return err
	}
	if waitErr != nil {
		return fmt.Errorf("authentication command: %w", waitErr)
	}
	return nil
}

func configuredExecutable(executables map[string]string, name string) string {
	if executable := strings.TrimSpace(executables[name]); executable != "" {
		return executable
	}
	return name
}

func terminateProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func ignorablePTYReadError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, syscall.EIO)
}

// expectedPrompt extracts only the device-login URL and human-entered code.
// Arbitrary provider output, including OAuth and refresh tokens, is discarded.
func expectedPrompt(provider task.Provider, value string) string {
	var urls []string
	var codes []string
	for _, line := range strings.Split(value, "\n") {
		lower := strings.ToLower(line)
		lineURLs := recoveryURLPattern.FindAllString(line, -1)
		if len(lineURLs) == 0 && (strings.Contains(lower, "token") || strings.Contains(lower, "bearer") || strings.Contains(lower, "refresh")) {
			continue
		}
		trustedLine := len(lineURLs) == 0
		codeSource := line
		for _, candidate := range lineURLs {
			codeSource = strings.ReplaceAll(codeSource, candidate, "")
			if !loginPromptLine(lower) {
				trustedLine = false
				continue
			}
			safe, ok := trustedRecoveryURL(provider, candidate)
			if !ok {
				trustedLine = false
				continue
			}
			trustedLine = true
			urls = append(urls, safe)
		}
		if trustedLine && (strings.Contains(lower, "enter") || strings.Contains(lower, "device code") || strings.Contains(lower, "one-time code")) {
			codes = append(codes, recoveryCodePattern.FindAllString(codeSource, -1)...)
		}
	}
	if len(urls) == 0 && len(codes) == 0 {
		return ""
	}
	var out strings.Builder
	for _, url := range unique(urls) {
		out.WriteString("URL: ")
		out.WriteString(url)
		out.WriteByte('\n')
	}
	for _, code := range unique(codes) {
		out.WriteString("Code: ")
		out.WriteString(code)
		out.WriteByte('\n')
	}
	return out.String()
}

func trustedRecoveryURL(provider task.Provider, candidate string) (string, bool) {
	candidate = strings.TrimRight(candidate, ".,;:)")
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !trustedAuthHost(provider, parsed.Hostname()) {
		return "", false
	}
	if !trustedAuthPath(provider, parsed.EscapedPath()) {
		return "", false
	}
	query := parsed.Query()
	for key := range query {
		switch strings.ToLower(key) {
		case "access_token", "refresh_token", "token", "id_token", "authorization", "oauth_token", "api_key", "code":
			return "", false
		case "client_id", "redirect_uri", "response_type", "scope", "state", "code_challenge", "code_challenge_method", "audience", "user_code":
		default:
			return "", false
		}
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), true
}

func loginPromptLine(line string) bool {
	return strings.Contains(line, "open ") || strings.Contains(line, "visit ") ||
		strings.Contains(line, "browse ") || strings.Contains(line, "go to ")
}

func trustedAuthPath(provider task.Provider, escapedPath string) bool {
	path, err := url.PathUnescape(escapedPath)
	if err != nil {
		return false
	}
	path = strings.ToLower(path)
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	switch provider {
	case task.ProviderCodex:
		switch path {
		case "/codex/device", "/device", "/oauth/authorize", "/oauth2/authorize", "/authorize", "/login", "/auth/login":
			return true
		}
	case task.ProviderClaude:
		switch path {
		case "/oauth/authorize", "/oauth2/authorize", "/authorize", "/login", "/auth/login":
			return true
		}
	}
	return false
}

func trustedAuthHost(provider task.Provider, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	var suffixes []string
	switch provider {
	case task.ProviderCodex:
		suffixes = []string{"openai.com", "chatgpt.com"}
	case task.ProviderClaude:
		suffixes = []string{"claude.ai", "anthropic.com"}
	default:
		return false
	}
	for _, suffix := range suffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
