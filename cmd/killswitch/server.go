// Owner: Claude
package main

import (
	"encoding/json"
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
}

// health is GET /health: liveness plus the tick counter and the M1 insert instrument; it never builds a World.
func (s *server) health(rw http.ResponseWriter, r *http.Request) {
	w := s.holder.Current()
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
		writeJSON(rw, http.StatusRequestEntityTooLarge, errorBody{Error: "bad_request", Tenant: tenant})
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
	w := s.holder.Current()
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
