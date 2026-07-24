package localcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// APIError is the stable error envelope returned by the authenticated local
// API. A Desktop client can branch on Code without parsing server text.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string { return fmt.Sprintf("local API: %s (%d)", e.Code, e.Status) }

type Client struct {
	baseURL string
	secret  []byte
	http    *http.Client
	close   func()
}

// NewClient constructs a client over an already configured HTTP transport.
// The client owns no provider process, SQLite handle, or filesystem path.
func NewClient(baseURL string, secret []byte, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" || len(secret) < 32 {
		return nil, ErrInvalidRequest
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), secret: append([]byte(nil), secret...), http: httpClient}, nil
}

// NewUnixClient connects a Desktop/control client to the owner-only AgentBridge
// Unix socket and reads the separately protected local API secret file.
func NewUnixClient(socketPath, secretPath string) (*Client, error) {
	if !filepath.IsAbs(socketPath) || !filepath.IsAbs(secretPath) {
		return nil, ErrInvalidRequest
	}
	info, err := os.Lstat(secretPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, ErrInvalidRequest
	}
	secret, err := os.ReadFile(secretPath)
	if err != nil || len(secret) < 32 {
		return nil, ErrInvalidRequest
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	client, err := NewClient("http://agentbridge", secret, &http.Client{Transport: transport})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	client.close = transport.CloseIdleConnections
	return client, nil
}

func (c *Client) Close() {
	if c != nil && c.close != nil {
		c.close()
	}
}

func (c *Client) CreateProject(ctx context.Context, request CreateProjectRequest) (ProjectResponse, error) {
	var response ProjectResponse
	err := c.do(ctx, http.MethodPost, "/v1/projects", request, &response)
	return response, err
}

func (c *Client) ListDevices(ctx context.Context) (DevicesResponse, error) {
	var response DevicesResponse
	err := c.do(ctx, http.MethodGet, "/v1/devices", nil, &response)
	return response, err
}

func (c *Client) CreatePairingChallenge(ctx context.Context, request CreatePairingChallengeRequest) (PairingChallengeResponse, error) {
	var response PairingChallengeResponse
	err := c.do(ctx, http.MethodPost, "/v1/devices/challenges", request, &response)
	return response, err
}

func (c *Client) PairDevice(ctx context.Context, request PairDeviceRequest) (DeviceResponse, error) {
	var response DeviceResponse
	err := c.do(ctx, http.MethodPost, "/v1/devices/pair", request, &response)
	return response, err
}

func (c *Client) ReplayDeviceCommands(ctx context.Context, request ReplayDeviceCommandsRequest) (ReplayDeviceCommandsResponse, error) {
	var response ReplayDeviceCommandsResponse
	err := c.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(request.DeviceID)+"/replay", request, &response)
	return response, err
}

func (c *Client) RotateDevice(ctx context.Context, request RotateDeviceRequest) (DeviceResponse, error) {
	var response DeviceResponse
	err := c.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(request.DeviceID)+"/rotate", request, &response)
	return response, err
}

func (c *Client) SetDeviceState(ctx context.Context, request DeviceMutationRequest, state DeviceState) (DeviceResponse, error) {
	var response DeviceResponse
	path := "/v1/devices/" + url.PathEscape(request.DeviceID) + "/" + string(state)
	if state == DeviceStatePaired {
		path = "/v1/devices/" + url.PathEscape(request.DeviceID) + "/reachable"
	} else if state == DeviceStateRevoked {
		path = "/v1/devices/" + url.PathEscape(request.DeviceID) + "/revoke"
	}
	err := c.do(ctx, http.MethodPost, path, request, &response)
	return response, err
}

func (c *Client) SelectTaskDevice(ctx context.Context, request SelectDeviceRequest) (AssignmentResponse, error) {
	var response AssignmentResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/device", request, &response)
	return response, err
}

func (c *Client) RegisterRepository(ctx context.Context, request RegisterRepositoryRequest) (RepositoryResponse, error) {
	var response RepositoryResponse
	err := c.do(ctx, http.MethodPost, "/v1/repositories", request, &response)
	return response, err
}

func (c *Client) CreateBoard(ctx context.Context, request CreateBoardRequest) (BoardResponse, error) {
	var response BoardResponse
	err := c.do(ctx, http.MethodPost, "/v1/boards", request, &response)
	return response, err
}

func (c *Client) CreateTask(ctx context.Context, request CreateTaskRequest) (TaskResponse, error) {
	var response TaskResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks", request, &response)
	return response, err
}

func (c *Client) UpdateTask(ctx context.Context, request UpdateTaskRequest) (ActionResponse, error) {
	var response ActionResponse
	err := c.doWithHeaders(ctx, http.MethodPatch, "/v1/tasks/"+url.PathEscape(request.TaskID), request, &response, map[string]string{
		"If-Match": fmt.Sprintf("\"%d\"", request.Revision),
	})
	return response, err
}

func (c *Client) Start(ctx context.Context, request StartRequest) (ActionResponse, error) {
	var response ActionResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/start", request, &response)
	return response, err
}

func (c *Client) Resume(ctx context.Context, request ResumeRequest) (ActionResponse, error) {
	var response ActionResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/resume", request, &response)
	return response, err
}

func (c *Client) Observe(ctx context.Context, request ObserveRequest) (ObserveResponse, error) {
	path := "/v1/tasks/" + url.PathEscape(request.TaskID) + "/events"
	query := url.Values{}
	if request.AfterCursor > 0 {
		query.Set("after_cursor", fmt.Sprint(request.AfterCursor))
	}
	if request.Limit > 0 {
		query.Set("limit", fmt.Sprint(request.Limit))
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response ObserveResponse
	err := c.do(ctx, http.MethodGet, path, nil, &response)
	return response, err
}

func (c *Client) PendingApprovals(ctx context.Context, taskID string) (ApprovalsResponse, error) {
	var response ApprovalsResponse
	err := c.do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(taskID)+"/approvals", nil, &response)
	return response, err
}

func (c *Client) Approve(ctx context.Context, request ApproveRequest) (ActionResponse, error) {
	var response ActionResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/approve", request, &response)
	return response, err
}

func (c *Client) Cancel(ctx context.Context, request CancelRequest) (ActionResponse, error) {
	var response ActionResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/cancel", request, &response)
	return response, err
}

func (c *Client) Verify(ctx context.Context, request VerifyRequest) (VerifyResponse, error) {
	var response VerifyResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/verify", request, &response)
	return response, err
}

func (c *Client) Commit(ctx context.Context, request CommitRequest) (CommitResponse, error) {
	var response CommitResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+url.PathEscape(request.TaskID)+"/commit", request, &response)
	return response, err
}

func (c *Client) do(ctx context.Context, method, path string, input, output any) error {
	return c.doWithHeaders(ctx, method, path, input, output, nil)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, input, output any, headers map[string]string) error {
	if c == nil || c.http == nil || len(c.secret) < 32 {
		return ErrNotConfigured
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("X-AgentBridge-Local-Auth", string(c.secret))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLocalRequestBytes))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var value struct {
			Code string `json:"error"`
		}
		_ = json.Unmarshal(data, &value)
		if value.Code == "" {
			value.Code = "request_failed"
		}
		return &APIError{Status: response.StatusCode, Code: value.Code}
	}
	if output == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode local API response: %w", err)
	}
	return nil
}
