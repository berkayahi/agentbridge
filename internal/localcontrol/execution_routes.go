package localcontrol

import (
	"net/http"

	"github.com/berkayahi/agentbridge/internal/executioncontract"
)

func (a *API) createExecution(w http.ResponseWriter, r *http.Request) {
	var request executioncontract.ExecutionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.CreateExecution(r.Context(), request)
	writeResult(w, http.StatusAccepted, response, err)
}

func (a *API) getExecution(w http.ResponseWriter, r *http.Request) {
	response, err := a.service.GetExecution(r.Context(), r.PathValue("id"))
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) saveExecutionResult(w http.ResponseWriter, r *http.Request) {
	var request ExecutionResultRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.SaveExecutionResult(r.Context(), r.PathValue("id"), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) recoverExecutions(w http.ResponseWriter, r *http.Request) {
	executions, err := a.service.RecoverExecutions(r.Context())
	writeResult(w, http.StatusOK, map[string]any{"executions": executions}, err)
}

func (a *API) acquireResourceLease(w http.ResponseWriter, r *http.Request) {
	var request executioncontract.ResourceLeaseRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.AcquireResourceLease(r.Context(), request)
	writeResult(w, http.StatusCreated, response, err)
}

func (a *API) heartbeatResourceLease(w http.ResponseWriter, r *http.Request) {
	var request ResourceLeaseHeartbeatRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	response, err := a.service.HeartbeatResourceLease(r.Context(), r.PathValue("id"), request)
	writeResult(w, http.StatusOK, response, err)
}

func (a *API) releaseResourceLease(w http.ResponseWriter, r *http.Request) {
	var request ResourceLeaseReleaseRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	err := a.service.ReleaseResourceLease(r.Context(), r.PathValue("id"), request)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

func (a *API) expiredResourceLeases(w http.ResponseWriter, r *http.Request) {
	leases, err := a.service.ExpiredResourceLeases(r.Context())
	writeResult(w, http.StatusOK, map[string]any{"leases": leases}, err)
}
