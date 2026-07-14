package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientDispatchesResponsesNotificationsAndServerRequests(t *testing.T) {
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	client := NewClient(clientReader, clientWriter, ClientOptions{})
	t.Cleanup(func() { _ = client.Close() })

	go func() {
		line, _ := bufio.NewReader(serverReader).ReadBytes('\n')
		var request wireMessage
		_ = json.Unmarshal(line, &request)
		fmt.Fprintf(serverWriter, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true},"future":"ignored"}`+"\n", request.ID)
		fmt.Fprintln(serverWriter, `{"jsonrpc":"2.0","method":"turn/started","params":{"turn":{"id":"turn-1"}}}`)
		fmt.Fprintln(serverWriter, `{"jsonrpc":"2.0","id":"approval-1","method":"item/commandExecution/requestApproval","params":{"command":"go test ./..."}}`)
	}()

	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.Call(context.Background(), "account/read", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatal("response was not decoded")
	}
	select {
	case notification := <-client.Notifications():
		if notification.Method != "turn/started" {
			t.Fatalf("notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not dispatched")
	}
	select {
	case request := <-client.Requests():
		if request.Method != "item/commandExecution/requestApproval" || request.ID != "approval-1" {
			t.Fatalf("request = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("server request not dispatched")
	}
}

func TestClientRejectsMalformedDuplicateAndUnknownResponseIDs(t *testing.T) {
	for _, input := range []string{
		"not-json\n",
		`{"jsonrpc":"2.0","id":"missing","result":{}}` + "\n",
	} {
		client := NewClient(strings.NewReader(input), io.Discard, ClientOptions{})
		select {
		case err := <-client.Errors():
			if err == nil {
				t.Fatal("nil protocol error")
			}
		case <-time.After(time.Second):
			t.Fatalf("no protocol error for %q", input)
		}
		_ = client.Close()
	}

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	client := NewClient(clientReader, clientWriter, ClientOptions{})
	defer client.Close()
	go func() {
		line, _ := bufio.NewReader(serverReader).ReadBytes('\n')
		var request wireMessage
		_ = json.Unmarshal(line, &request)
		fmt.Fprintf(serverWriter, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
		fmt.Fprintf(serverWriter, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
	}()
	if err := client.Call(context.Background(), "account/read", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-client.Errors():
		if !errors.Is(err, ErrUnknownResponse) {
			t.Fatalf("error = %v, want ErrUnknownResponse", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate response was not reported")
	}
}

func TestCallHonorsCancellationAndPendingBound(t *testing.T) {
	clientReader, serverWriter := io.Pipe()
	_, clientWriter := io.Pipe()
	client := NewClient(clientReader, clientWriter, ClientOptions{MaxPending: 1})
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		_ = client.Call(ctx, "thread/start", nil, nil)
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for client.Pending() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := client.Call(context.Background(), "thread/resume", nil, nil); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("error = %v, want ErrTooManyPending", err)
	}
	cancel()
	_ = serverWriter.Close()
}

func TestProcessInitializesAndFailsPendingCallsOnExit(t *testing.T) {
	if os.Getenv("GO_WANT_CODEX_HELPER") == "1" {
		runCodexHelper()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := StartProcess(ctx, ProcessConfig{
		Executable: os.Args[0],
		Args:       []string{"-test.run=TestProcessInitializesAndFailsPendingCallsOnExit", "--"},
		Env:        append(os.Environ(), "GO_WANT_CODEX_HELPER=1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var account map[string]any
	if err := p.Client.Call(ctx, "account/read", map[string]any{}, &account); err != nil {
		t.Fatal(err)
	}
	if account["type"] != "chatgpt" {
		t.Fatalf("account = %#v", account)
	}
	err = p.Client.Call(ctx, "wait/for/exit", nil, nil)
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("error = %v, want ErrProcessExited", err)
	}
	if strings.Contains(p.Stderr(), "secret-value") {
		t.Fatalf("stderr leaked secret: %q", p.Stderr())
	}
}

func TestOfficialAppServerArguments(t *testing.T) {
	got := AppServerArgs()
	want := []string{"app-server", "--listen", "stdio://"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func runCodexHelper() {
	scanner := bufio.NewScanner(os.Stdin)
	var writeMu sync.Mutex
	write := func(value string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintln(os.Stdout, value)
	}
	for scanner.Scan() {
		var request wireMessage
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		switch request.Method {
		case "initialize":
			write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"fake-codex"}}`, request.ID))
		case "account/read":
			write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"type":"chatgpt"}}`, request.ID))
		case "wait/for/exit":
			fmt.Fprintln(os.Stderr, "OPENAI_API_KEY=secret-value")
			return
		}
	}
}

func TestFixturesAreSanitizedJSONLines(t *testing.T) {
	data, err := os.ReadFile("testdata/turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("api_key")) || bytes.Contains(data, []byte("token")) {
		t.Fatal("fixture contains credential-like data")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var value any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			t.Fatalf("invalid JSONL fixture: %v", err)
		}
	}
}
