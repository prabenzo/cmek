// Owner: Claude (reviewed by Ben)
package world

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/prabenzo/cmek/internal/admit"
	"github.com/prabenzo/cmek/internal/cmek"
)

// ErrUnknownTenant is returned for an id outside the population (404).
var ErrUnknownTenant = errors.New("world: unknown tenant")

// Ingest is the single front door used by the HTTP handler and the load generator.
func (w *World) Ingest(ctx context.Context, tenant string, payload []byte) error {
	_, err := w.IngestID(ctx, tenant, payload)
	return err
}

// reasonFor is the metrics reason for an Ingest outcome, derived from Outcome so the two never disagree.
func reasonFor(err error) string {
	_, reason, _ := Outcome(err, RetryAfter{})
	if reason == "" {
		return "accepted"
	}
	return reason
}

// reject counts a failed ingest by reason and share class; an internal error (not an admission or key-state
// answer) is also logged, throttled.
func (w *World) reject(idx int, within bool, step string, err error) error {
	reason := reasonFor(err)
	w.metrics.Ingest(idx, reason, within)
	if reason == "internal" {
		w.errLog.Error("ingest failed", "step", step, "tenant", w.ids[idx], "err", err)
	}
	return err
}

// IngestID is Ingest plus the allocated message id for the 202 body: admit, key, seal, insert, count the outcome.
// Admission runs first and cheapest, so a rejected event costs no KMS call and no data-key message; the share class
// it decided on is what the counters record (no second backlog read, no override).
func (w *World) IngestID(ctx context.Context, tenant string, payload []byte) (int64, error) {
	idx, ok := w.index[tenant]
	if !ok {
		return 0, ErrUnknownTenant
	}
	within, err := w.admit.Admit(idx)
	if err != nil {
		return 0, w.reject(idx, within, "admit", err)
	}
	h, err := w.keys.EncryptKey(ctx, tenant)
	if err != nil {
		return 0, w.reject(idx, within, "encrypt key", err)
	}
	id := w.store.NextID()
	env, err := cmek.Seal(h, w.clock.Now(), tenant, id, payload)
	h.Zero()
	if err != nil {
		return 0, w.reject(idx, within, "seal", err)
	}
	if err := w.store.Insert(ctx, idx, id, env); err != nil {
		return 0, w.reject(idx, within, "insert", fmt.Errorf("world: insert: %w", err))
	}
	w.metrics.Ingest(idx, "accepted", within)
	return id, nil
}

// Outcome maps an Ingest error to HTTP status, JSON reason and the fixed Retry-After seconds (0 = none) by sentinel; the only mapping in the program.
func Outcome(err error, ra RetryAfter) (status int, reason string, retryAfter int) {
	switch {
	case err == nil:
		return http.StatusAccepted, "", 0
	case errors.Is(err, ErrUnknownTenant):
		return http.StatusNotFound, "unknown_tenant", 0
	case errors.Is(err, admit.ErrOverloaded):
		return http.StatusServiceUnavailable, "overloaded", ra.Overloaded
	case errors.Is(err, admit.ErrRateLimited):
		return http.StatusTooManyRequests, "rate_limited", ra.RateLimited
	case errors.Is(err, admit.ErrBacklogFull):
		return http.StatusTooManyRequests, "backlog_full", ra.BacklogFull
	case errors.Is(err, cmek.ErrKeyRevoked):
		return http.StatusForbidden, "key_revoked", 0
	case errors.Is(err, cmek.ErrKeyUnavailable), errors.Is(err, cmek.ErrLeaseExpired):
		return http.StatusServiceUnavailable, "key_unavailable", ra.KeyUnavailable
	}
	return http.StatusInternalServerError, "internal", 0
}
