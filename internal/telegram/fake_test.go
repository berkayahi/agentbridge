package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

type botRequest struct {
	Method string
	Values map[string]any
}

type fakeBotAPI struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []botRequest
	handler  func(string, http.ResponseWriter, *http.Request)
}

func newFakeBotAPI(t interface{ Fatal(...any) }) *fakeBotAPI {
	f := &fakeBotAPI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[stringsLastSlash(r.URL.Path)+1:]
		if f.handler != nil {
			f.handler(method, w, r)
			return
		}
		values := decodeBotRequest(r)
		f.mu.Lock()
		f.requests = append(f.requests, botRequest{Method: method, Values: values})
		f.mu.Unlock()
		result := any(true)
		if method == "sendMessage" || method == "editMessageText" || method == "sendDocument" {
			result = map[string]any{"message_id": 77, "date": 1, "chat": map[string]any{"id": 100, "type": "private"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	return f
}

func decodeBotRequest(r *http.Request) map[string]any {
	values := make(map[string]any)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return values
	}
	for key, entries := range r.MultipartForm.Value {
		if len(entries) == 0 {
			continue
		}
		var decoded any
		if json.Unmarshal([]byte(entries[0]), &decoded) == nil {
			values[key] = decoded
		} else {
			values[key] = entries[0]
		}
	}
	return values
}

func stringsLastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func (f *fakeBotAPI) URL() string { return f.server.URL }
func (f *fakeBotAPI) Close()      { f.server.Close() }
func (f *fakeBotAPI) Methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	methods := make([]string, len(f.requests))
	for i, r := range f.requests {
		methods[i] = r.Method
	}
	return methods
}
func writeAPIError(w http.ResponseWriter, code int, description string, retryAfter int) {
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":%q,"parameters":{"retry_after":%d}}`, code, description, retryAfter)
}
