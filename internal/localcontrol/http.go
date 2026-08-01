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

	"github.com/berkayahi/agentbridge/internal/advisory"
	"github.com/berkayahi/agentbridge/internal/repositorysnapshot"
	"github.com/berkayahi/agentbridge/internal/store"
	"github.com/berkayahi/agentbridge/internal/workmodel"
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
	mux.HandleFunc("GET /v1/events", a.observeHive)
	mux.HandleFunc("GET /v1/projects", a.listProjects)
	mux.HandleFunc("POST /v1/projects", a.createProject)
	mux.HandleFunc("GET /v1/boards", a.listBoards)
	mux.HandleFunc("GET /v1/tasks", a.listTasks)
	mux.HandleFunc("GET /v1/providers", a.listProviders)
	mux.HandleFunc("GET /v1/usage", a.usage)
	mux.HandleFunc("GET /v1/analytics/usage", a.usageAnalytics)
	mux.HandleFunc("GET /v1/repositories", a.listRepositories)
	mux.HandleFunc("POST /v1/repository-snapshots", a.createRepositorySnapshot)
	mux.HandleFunc("POST /v1/repository-understanding", a.createRepositoryUnderstanding)
	mux.HandleFunc("POST /v1/advisory-sessions", a.createAdvisorySession)
	mux.HandleFunc("POST /v1/repositories", a.registerRepository)
	mux.HandleFunc("POST /v1/repositories/configure", a.configureRepository)
	mux.HandleFunc("POST /v1/repositories/{id}/integrate", a.integrateRepository)
	mux.HandleFunc("POST /v1/boards", a.createBoard)
	mux.HandleFunc("POST /v1/tasks", a.createTask)
	mux.HandleFunc("PATCH /v1/tasks/{id}", a.updateTask)
	mux.HandleFunc("GET /v1/tasks/{id}", a.getTask)
	mux.HandleFunc("GET /v1/tasks/{id}/events", a.observe)
	mux.HandleFunc("GET /v1/tasks/{id}/approvals", a.pendingApprovals)
	mux.HandleFunc("GET /v1/tasks/{id}/diff", a.taskDiff)
	mux.HandleFunc("POST /v1/tasks/{id}/start", a.start)
	mux.HandleFunc("POST /v1/tasks/{id}/continue", a.continueTask)
	mux.HandleFunc("POST /v1/tasks/{id}/resume", a.resume)
	mux.HandleFunc("POST /v1/tasks/{id}/steer", a.steer)
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

func (a *API) createRepositorySnapshot(w http.ResponseWriter, r *http.Request) {
	var request RepositorySnapshotRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.CreateRepositorySnapshot(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) createRepositoryUnderstanding(w http.ResponseWriter, r *http.Request) {
	var request RepositoryUnderstandingRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.UnderstandRepository(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) createAdvisorySession(w http.ResponseWriter, r *http.Request) {
	var request advisory.SessionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.ExecuteAdvisorySession(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) integrateRepository(w http.ResponseWriter, r *http.Request) {
	var request IntegrateRepositoryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.RepositoryID = r.PathValue("id")
	response, err := a.service.IntegrateRepository(r.Context(), request)
	writeResult(w, http.StatusOK, response, err)
}

// usage is deliberately separate from the provider catalog. The catalog is
// polled every couple of seconds by the surface; asking a runtime what a
// subscription has left is not that cheap.
func (a *API) usage(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.Usage(r.Context())
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) usageAnalytics(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.UsageAnalytics(r.Context(), strings.TrimSpace(r.URL.Query().Get("project_id")))
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) configureRepository(w http.ResponseWriter, r *http.Request) {
	var request ConfigureRepositoryRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.ConfigureRepository(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	// A host version-gates the engine before it trusts this contract, so the
	// API version is reported alongside liveness.
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "local_api_version": APIVersion})
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

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.ListProjects(r.Context())
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) listBoards(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.ListBoards(r.Context(), strings.TrimSpace(r.URL.Query().Get("project_id")))
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := TaskFilter{
		ProjectID:      strings.TrimSpace(query.Get("project_id")),
		BoardID:        strings.TrimSpace(query.Get("board_id")),
		RepositoryID:   strings.TrimSpace(query.Get("repository_id")),
		TargetDeviceID: strings.TrimSpace(query.Get("device_id")),
		Limit:          parseLimit(query.Get("limit")),
	}
	for _, state := range query["state"] {
		value := workmodel.State(strings.TrimSpace(state))
		if !value.Valid() {
			writeServiceError(w, ErrInvalidRequest)
			return
		}
		filter.States = append(filter.States, value)
	}
	response, err := a.service.ListTasks(r.Context(), filter)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) observeHive(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("after_cursor"))
	after, err := strconv.ParseUint(raw, 10, 64)
	if err != nil && raw != "" {
		writeError(w, http.StatusBadRequest, "invalid_cursor")
		return
	}
	response, serviceErr := a.service.ObserveHive(r.Context(), after, parseLimit(r.URL.Query().Get("limit")))
	writeResult(w, http.StatusOK, response, serviceErr)
}

func (a *API) listProviders(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.ListProviders(r.Context())
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) listRepositories(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.ListRepositories(r.Context())
	writeResult(w, http.StatusOK, response, err)
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

func (a *API) continueTask(w http.ResponseWriter, r *http.Request) {
	var request ContinueTaskRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.ParentTaskID = r.PathValue("id")
	response, err := a.service.ContinueTask(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) taskDiff(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.TaskDiff(r.Context(), r.PathValue("id"))
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) steer(w http.ResponseWriter, r *http.Request) {
	var request SteerRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	request.TaskID = r.PathValue("id")
	response, err := a.service.Steer(r.Context(), request)
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
	case errors.Is(err, advisory.ErrInvalidRequest):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, advisory.ErrPolicyViolation):
		status, code = http.StatusForbidden, "policy_violation"
	case errors.Is(err, advisory.ErrStructuredOutput), errors.Is(err, advisory.ErrProviderOutputBounds), errors.Is(err, advisory.ErrProviderIdentity), errors.Is(err, advisory.ErrReceiptIntegrity):
		status, code = http.StatusBadGateway, "provider_output_invalid"
	case errors.Is(err, advisory.ErrNotConfigured):
		status, code = http.StatusServiceUnavailable, "provider_not_configured"
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, repositorysnapshot.ErrInvalidRequest),
		errors.Is(err, repositorysnapshot.ErrRefNotAllowed), errors.Is(err, repositorysnapshot.ErrScopeNotFound),
		errors.Is(err, repositorysnapshot.ErrPathNotAllowed), errors.Is(err, repositorysnapshot.ErrPathNotFound),
		errors.Is(err, repositorysnapshot.ErrUnknownRole):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, repositorysnapshot.ErrCommitMismatch):
		status, code = http.StatusConflict, "exact_commit_mismatch"
	case errors.Is(err, repositorysnapshot.ErrNotConfigured):
		status, code = http.StatusNotFound, "repository_not_configured"
	case errors.Is(err, repositorysnapshot.ErrProviderNotConfigured), errors.Is(err, repositorysnapshot.ErrProviderPolicy):
		status, code = http.StatusServiceUnavailable, "provider_not_configured"
	case errors.Is(err, repositorysnapshot.ErrProviderApproval):
		status, code = http.StatusForbidden, "provider_approval_declined"
	case errors.Is(err, repositorysnapshot.ErrProviderOutput), errors.Is(err, repositorysnapshot.ErrProviderOutputBounds):
		status, code = http.StatusBadGateway, "provider_output_invalid"
	case errors.Is(err, repositorysnapshot.ErrSecretLikeFile), errors.Is(err, repositorysnapshot.ErrBinaryEvidence), errors.Is(err, repositorysnapshot.ErrEvidenceMissing):
		status, code = http.StatusUnprocessableEntity, "evidence_unavailable"
	case errors.Is(err, repositorysnapshot.ErrBoundsExceeded):
		status, code = http.StatusUnprocessableEntity, "snapshot_bounds_exceeded"
	case errors.Is(err, repositorysnapshot.ErrConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, ErrUnknownProvider):
		status, code = http.StatusBadRequest, "unknown_provider"
	case errors.Is(err, ErrRepositoryNotConfigured):
		status, code = http.StatusBadRequest, "repository_not_configured"
	case errors.Is(err, ErrRepositoryAmbiguous):
		status, code = http.StatusConflict, "repository_ambiguous"
	case errors.Is(err, ErrDeliveryNotEnabled):
		status, code = http.StatusConflict, "delivery_not_enabled"
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
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrInvalidTransition), errors.Is(err, ErrVerificationRequired), errors.Is(err, ErrCommitRequired),
		errors.Is(err, ErrTaskOwnedByAnotherController):
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
