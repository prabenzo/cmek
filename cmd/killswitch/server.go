// Owner: Claude
package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/prabenzo/cmek/internal/world"
)

const maxBody = 64 << 10

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

// routes registers every API handler (Go 1.22 patterns).
func (s *server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /v1/stream", s.stream)
	mux.HandleFunc("POST /v1/events", s.events)
	mux.HandleFunc("GET /v1/tenants/{id}", s.tenant)
	mux.HandleFunc("POST /v1/faults", s.faults)
	mux.HandleFunc("POST /v1/tenants/{id}/key", s.key)
	mux.HandleFunc("POST /v1/traffic", s.traffic)
	mux.HandleFunc("POST /v1/scenarios/{name}/start", s.scenarioStart)
	mux.HandleFunc("POST /v1/scenarios/{name}/stop", s.scenarioStop)
	mux.HandleFunc("POST /v1/reset", s.reset)
}

// reset is POST /v1/reset: stop the current World and build a fresh one; 200 {"world":"w-n"}. It is the one
// handler that does not go through Ensure: holding a release while asking for the reset would deadlock by
// construction (Reset's Stop waits for released handlers).
func (s *server) reset(rw http.ResponseWriter, r *http.Request) {
	w := s.holder.Reset()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"world": w.ID})
}

// traffic is POST /v1/traffic {"tenant":"t-0042"|"","multiplier":5}: one tenant's or everyone's offered rate
// (200 echoes the body; 400 on bad JSON or multiplier ≤ 0; 404 unknown_tenant).
func (s *server) traffic(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Tenant     string  `json:"tenant"`
		Multiplier float64 `json:"multiplier"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, maxBody)).Decode(&req); err != nil || req.Multiplier <= 0 {
		writeJSON(rw, http.StatusBadRequest, errorBody{Error: "bad_request", Tenant: req.Tenant})
		return
	}
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	if err := w.Surge(req.Tenant, req.Multiplier); err != nil {
		s.controlOutcome(rw, err, req.Tenant)
		return
	}
	writeJSON(rw, http.StatusOK, req)
}

// scenarioStart is POST /v1/scenarios/{name}/start: 200 {"scenario":name}; 409 busy; 404 unknown_scenario.
func (s *server) scenarioStart(rw http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	switch err := w.StartScenario(name); {
	case err == nil:
		writeJSON(rw, http.StatusOK, map[string]string{"scenario": name})
	case errors.Is(err, world.ErrBusy):
		writeJSON(rw, http.StatusConflict, errorBody{Error: "busy"})
	case errors.Is(err, world.ErrUnknownScenario):
		writeJSON(rw, http.StatusNotFound, errorBody{Error: "unknown_scenario"})
	default:
		writeJSON(rw, http.StatusBadRequest, errorBody{Error: "bad_request"})
	}
}

// scenarioStop is POST /v1/scenarios/{name}/stop: cancels whatever runs and returns 200 with the name of the
// scenario that was actually stopped ("" while idle), whatever the path said.
func (s *server) scenarioStop(rw http.ResponseWriter, r *http.Request) {
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"scenario": w.StopScenario()})
}

// faults is POST /v1/faults: install or clear a fault on a provider or a tenant (204; 400 bad_request; 404 unknown_tenant).
func (s *server) faults(rw http.ResponseWriter, r *http.Request) {
	var req world.FaultRequest
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, maxBody)).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorBody{Error: "bad_request"})
		return
	}
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	s.controlOutcome(rw, w.Fault(req), req.Tenant)
}

// key is POST /v1/tenants/{id}/key {"action":"revoke"|"restore"} (204; 400; 404).
func (s *server) key(rw http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, maxBody)).Decode(&req); err != nil {
		writeJSON(rw, http.StatusBadRequest, errorBody{Error: "bad_request"})
		return
	}
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	s.controlOutcome(rw, w.SetKey(r.PathValue("id"), req.Action), r.PathValue("id"))
}

func (s *server) controlOutcome(rw http.ResponseWriter, err error, tenant string) {
	switch {
	case err == nil:
		rw.WriteHeader(http.StatusNoContent)
	case errors.Is(err, world.ErrUnknownTenant):
		writeJSON(rw, http.StatusNotFound, errorBody{Error: "unknown_tenant", Tenant: tenant})
	default:
		writeJSON(rw, http.StatusBadRequest, errorBody{Error: "bad_request", Tenant: tenant})
	}
}

// health is GET /health: liveness plus the tick counter and the M1 insert instrument; it never builds a World.
func (s *server) health(rw http.ResponseWriter, r *http.Request) {
	w, release := s.holder.Current()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]string{"error": "no_world"})
		return
	}
	writeJSON(rw, http.StatusOK, w.Health())
}

type errorBody struct {
	Error      string `json:"error"`
	Tenant     string `json:"tenant,omitempty"`
	RetryAfter int    `json:"retry_after_s,omitempty"`
}

// events is POST /v1/events with header X-Tenant-ID: the same Ingest the load generator calls, mapped through world.Outcome.
func (s *server) events(rw http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get("X-Tenant-ID")
	if tenant == "" {
		writeJSON(rw, http.StatusBadRequest, errorBody{Error: "bad_request"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxBody))
	if err != nil {
		status := http.StatusBadRequest // an aborted or malformed body
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(rw, status, errorBody{Error: "bad_request", Tenant: tenant})
		return
	}
	w, release := s.holder.Ensure()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	id, err := w.IngestID(r.Context(), tenant, body)
	status, reason, ra := world.Outcome(err, w.P.RetryAfter)
	if err == nil {
		writeJSON(rw, status, map[string]any{"id": id, "tenant": tenant})
		return
	}
	if ra > 0 {
		rw.Header().Set("Retry-After", strconv.Itoa(ra))
	}
	writeJSON(rw, status, errorBody{Error: reason, Tenant: tenant, RetryAfter: ra})
}

// tenant is GET /v1/tenants/{id}: the tenant read model (M1: state, rank, backlog, offered rate; M4 fills the rest).
func (s *server) tenant(rw http.ResponseWriter, r *http.Request) {
	w, release := s.holder.Current()
	defer release()
	if w == nil {
		writeJSON(rw, http.StatusServiceUnavailable, errorBody{Error: "no_world"})
		return
	}
	d, err := w.Tenant(r.PathValue("id"))
	if err != nil {
		writeJSON(rw, http.StatusNotFound, errorBody{Error: "unknown_tenant"})
		return
	}
	writeJSON(rw, http.StatusOK, d)
}
