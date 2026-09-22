// Owner: Claude (reviewed by Ben)
package world

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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

// IngestID is Ingest plus the allocated message id for the 202 body: key, seal, insert, count the outcome (admission is inserted ahead of the key in M3).
func (w *World) IngestID(ctx context.Context, tenant string, payload []byte) (int64, error) {
	idx, ok := w.index[tenant]
	if !ok {
		return 0, ErrUnknownTenant
	}
	h, err := w.keys.EncryptKey(ctx, tenant)
	if err != nil {
		w.metrics.Ingest(idx, reasonFor(err), true)
		return 0, err
	}
	id := w.store.NextID()
	env, err := cmek.Seal(h, w.clock.Now(), tenant, id, payload)
	h.Zero()
	if err != nil {
		w.metrics.Ingest(idx, reasonFor(err), true)
		return 0, err
	}
	if err := w.store.Insert(ctx, idx, id, env); err != nil {
		err = fmt.Errorf("world: insert: %w", err)
		w.metrics.Ingest(idx, reasonFor(err), true)
		return 0, err
	}
	w.metrics.Ingest(idx, "accepted", true)
	return id, nil
}

// Outcome maps an Ingest error to HTTP status, JSON reason and the fixed Retry-After seconds (0 = none) by sentinel; the only mapping in the program.
func Outcome(err error, ra RetryAfter) (status int, reason string, retryAfter int) {
	switch {
	case err == nil:
		return http.StatusAccepted, "", 0
	case errors.Is(err, ErrUnknownTenant):
		return http.StatusNotFound, "unknown_tenant", 0
	case errors.Is(err, cmek.ErrKeyRevoked):
		return http.StatusForbidden, "key_revoked", 0
	case errors.Is(err, cmek.ErrKeyUnavailable), errors.Is(err, cmek.ErrLeaseExpired):
		return http.StatusServiceUnavailable, "key_unavailable", ra.KeyUnavailable
	}
	return http.StatusInternalServerError, "internal", 0
}
