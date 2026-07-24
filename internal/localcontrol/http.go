package localcontrol

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/berkayahi/agentbridge/internal/store"
)

const maxLocalRequestBytes = 1 << 20

type API struct {
	service *Service
	secret  []byte
}

func NewAPI(service *Service, secret []byte) (*API, error) {
	if service == nil || len(secret) < 32 {
		return nil, ErrInvalidRequest
	}
	return &API{service: service, secret: append([]byte(nil), secret...)}, nil
}

func NewHTTPHandler(service *Service, secret []byte) (http.Handler, error) {
	api, err := NewAPI(service, secret)
	if err != nil {
		return nil, err
	}
	return api.Handler(), nil
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /v1/devices", a.listDevices)
	mux.HandleFunc("POST /v1/devices/challenges", a.createPairingChallenge)
	mux.HandleFunc("POST /v1/devices/pair", a.pairDevice)
	mux.HandleFunc("POST /v1/devices/{id}/replay", a.replayDeviceCommands)
	mux.HandleFunc("POST /v1/devices/{id}/rotate", a.rotateDevice)
	mux.HandleFunc("POST /v1/devices/{id}/reachable", a.reachableDevice)
	mux.HandleFunc("POST /v1/devices/{id}/unreachable", a.unreachableDevice)
	mux.HandleFunc("POST /v1/devices/{id}/revoke", a.revokeDevice)
	mux.HandleFunc("POST /v1/projects", a.createProject)
	mux.HandleFunc("POST /v1/repositories", a.registerRepository)
	mux.HandleFunc("POST /v1/boards", a.createBoard)
	mux.HandleFunc("POST /v1/tasks", a.createTask)
	mux.HandleFunc("PATCH /v1/tasks/{id}", a.updateTask)
	mux.HandleFunc("GET /v1/tasks/{id}", a.getTask)
	mux.HandleFunc("GET /v1/tasks/{id}/events", a.observe)
	mux.HandleFunc("GET /v1/tasks/{id}/approvals", a.pendingApprovals)
	mux.HandleFunc("POST /v1/tasks/{id}/start", a.start)
	mux.HandleFunc("POST /v1/tasks/{id}/resume", a.resume)
	mux.HandleFunc("POST /v1/tasks/{id}/approve", a.approve)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", a.cancel)
	mux.HandleFunc("POST /v1/tasks/{id}/verify", a.verify)
	mux.HandleFunc("POST /v1/tasks/{id}/commit", a.commit)
	mux.HandleFunc("POST /v1/tasks/{id}/device", a.selectTaskDevice)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !a.authorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.ListDevices(r.Context())
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) createPairingChallenge(w http.ResponseWriter, r *http.Request) {
	var request CreatePairingChallengeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.CreatePairingChallenge(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) pairDevice(w http.ResponseWriter, r *http.Request) {
	var request PairDeviceRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.PairDevice(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) replayDeviceCommands(w http.ResponseWriter, r *http.Request) {
	var request ReplayDeviceCommandsRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.DeviceID = r.PathValue("id")
	response, err := a.service.ReplayDeviceCommands(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) rotateDevice(w http.ResponseWriter, r *http.Request) {
	var request RotateDeviceRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.DeviceID = r.PathValue("id")
	response, err := a.service.RotateDevice(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) reachableDevice(w http.ResponseWriter, r *http.Request) {
	a.setDeviceState(w, r, DeviceStatePaired)
}

func (a *API) unreachableDevice(w http.ResponseWriter, r *http.Request) {
	a.setDeviceState(w, r, DeviceStateUnreachable)
}

func (a *API) revokeDevice(w http.ResponseWriter, r *http.Request) {
	a.setDeviceState(w, r, DeviceStateRevoked)
}

func (a *API) setDeviceState(w http.ResponseWriter, r *http.Request, state DeviceState) {
	var request DeviceMutationRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.DeviceID = r.PathValue("id")
	response, err := a.service.SetDeviceState(r.Context(), request, state)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var request CreateProjectRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.CreateProject(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) registerRepository(w http.ResponseWriter, r *http.Request) {
	var request RegisterRepositoryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.RegisterRepository(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) createBoard(w http.ResponseWriter, r *http.Request) {
	var request CreateBoardRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.CreateBoard(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) createTask(w http.ResponseWriter, r *http.Request) {
	var request CreateTaskRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.CreateTask(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) updateTask(w http.ResponseWriter, r *http.Request) {
	var request UpdateTaskRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	if headerRevision, err := parseIfMatchRevision(r.Header.Get("If-Match")); err != nil {
		writeServiceError(w, err)
		return
	} else if headerRevision > 0 {
		if request.Revision > 0 && request.Revision != headerRevision {
			writeServiceError(w, ErrStaleRevision)
			return
		}
		request.Revision = headerRevision
	}
	response, err := a.service.UpdateTask(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) getTask(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.Observe(r.Context(), ObserveRequest{TaskID: r.PathValue("id"), Limit: 1})
	if err != nil {
		writeResult(w, http.StatusOK, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, TaskResponse{Task: response.Task})
}

func (a *API) observe(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(strings.TrimSpace(r.URL.Query().Get("after_cursor")), 10, 64)
	if err != nil && r.URL.Query().Get("after_cursor") != "" {
		writeError(w, http.StatusBadRequest, "invalid_cursor")
		return
	}
	response, serviceErr := a.service.Observe(r.Context(), ObserveRequest{TaskID: r.PathValue("id"), AfterCursor: after, Limit: parseLimit(r.URL.Query().Get("limit"))})
	writeResult(w, http.StatusOK, response, serviceErr)
}

func (a *API) pendingApprovals(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.PendingApprovals(r.Context(), r.PathValue("id"))
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) start(w http.ResponseWriter, r *http.Request) {
	var request StartRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.Start(r.Context(), request)
	writeResult(w, http.StatusAccepted, response, err)
}

func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	var request ApproveRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	// The local API is authenticated as one owner authority. Do not let a
	// browser-supplied user_id become a second authorization principal.
	request.UserID = LocalAuthorityUserID
	response, err := a.service.Approve(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) resume(w http.ResponseWriter, r *http.Request) {
	var request ResumeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.Resume(r.Context(), request)
	writeResult(w, http.StatusAccepted, response, err)
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	var request CancelRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.Cancel(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) verify(w http.ResponseWriter, r *http.Request) {
	var request VerifyRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.Verify(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) commit(w http.ResponseWriter, r *http.Request) {
	var request CommitRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.Commit(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) selectTaskDevice(w http.ResponseWriter, r *http.Request) {
	var request SelectDeviceRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.SelectTaskDevice(r.Context(), request)
	writeResult(w, http.StatusAccepted, response, err)
}

func (a *API) authorized(r *http.Request) bool {
	candidate := strings.TrimSpace(r.Header.Get("X-AgentBridge-Local-Auth"))
	if candidate == "" {
		candidate = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	if subtle.ConstantTimeCompare([]byte(candidate), a.secret) == 1 {
		return true
	}
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		decoded, err := encoding.DecodeString(candidate)
		if err == nil && subtle.ConstantTimeCompare(decoded, a.secret) == 1 {
			return true
		}
	}
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	defer r.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(r.Body, maxLocalRequestBytes+1))
	if err != nil || len(contents) > maxLocalRequestBytes {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func parseLimit(value string) int {
	if value == "" {
		return 100
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 100
	}
	if parsed > 200 {
		return 200
	}
	return parsed
}

func parseIfMatchRevision(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' || strings.HasPrefix(value, "W/") {
		return 0, ErrInvalidRequest
	}
	revision, err := strconv.ParseInt(value[1:len(value)-1], 10, 64)
	if err != nil || revision <= 0 {
		return 0, ErrInvalidRequest
	}
	return revision, nil
}

func writeResult(w http.ResponseWriter, successStatus int, value any, err error) {
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, successStatus, value)
}

func writeServiceError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "request_failed"
	switch {
	case errors.Is(err, ErrInvalidRequest):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, ErrUnknownProvider):
		status, code = http.StatusBadRequest, "unknown_provider"
	case errors.Is(err, ErrStaleRevision):
		status, code = http.StatusConflict, "stale_revision"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, ErrNotConfigured):
		status, code = http.StatusServiceUnavailable, "not_configured"
	case errors.Is(err, store.ErrNotFound), errors.Is(err, ErrApprovalNotPending):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrPairingExpired):
		status, code = http.StatusGone, "pairing_expired"
	case errors.Is(err, ErrPairingUsed):
		status, code = http.StatusConflict, "pairing_used"
	case errors.Is(err, ErrInvalidDeviceProof):
		status, code = http.StatusUnauthorized, "invalid_device_proof"
	case errors.Is(err, ErrDeviceUnreachable):
		status, code = http.StatusServiceUnavailable, "device_unreachable"
	case errors.Is(err, ErrDeviceRevoked):
		status, code = http.StatusForbidden, "device_revoked"
	case errors.Is(err, ErrDeviceNotPaired), errors.Is(err, ErrDeviceFence):
		status, code = http.StatusConflict, "device_not_ready"
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrInvalidTransition), errors.Is(err, ErrVerificationRequired), errors.Is(err, ErrCommitRequired):
		status, code = http.StatusConflict, "conflict"
	}
	writeError(w, status, code)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
