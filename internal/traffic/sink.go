// Owner: Claude
package traffic

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTenantMismatch is the S2 witness: the payload's canary named another tenant.
var ErrTenantMismatch = errors.New("sink: payload canary names another tenant")

// SinkConfig sizes the fake webhook endpoint.
type SinkConfig struct {
	Tenants                []string
	MinLatency, MaxLatency time.Duration
	Ring                   int // deliveredAt ring per tenant (M5)
	CanaryPrefix           string
	Clock                  Clock
	Rand                   *rand.Rand
	Lock                   *sync.Mutex
}

// Sink stamps deliveredAt on entry, verifies the canary's tenant, sleeps 5-20 ms, and records the delivery.
type Sink struct {
	cfg        SinkConfig
	count      []atomic.Int64
	mismatches atomic.Int64
}

// NewSink builds the sink.
func NewSink(cfg SinkConfig) *Sink {
	return &Sink{cfg: cfg, count: make([]atomic.Int64, len(cfg.Tenants))}
}

func (s *Sink) latency() time.Duration {
	s.cfg.Lock.Lock()
	u := s.cfg.Rand.Float64()
	s.cfg.Lock.Unlock()
	return s.cfg.MinLatency + time.Duration(u*float64(s.cfg.MaxLatency-s.cfg.MinLatency))
}

// Deliver accepts one plaintext delivery; the canary must name the delivering tenant (S2).
func (s *Sink) Deliver(ctx context.Context, tenant string, idx int, msgID int64, plaintext []byte) (time.Time, error) {
	at := s.cfg.Clock.Now()
	if !bytes.Contains(plaintext, []byte(`"`+s.cfg.CanaryPrefix+tenant+`"`)) {
		s.mismatches.Add(1)
		return at, ErrTenantMismatch
	}
	select {
	case <-s.cfg.Clock.After(s.latency()):
	case <-ctx.Done():
		return at, ctx.Err()
	}
	s.count[idx].Add(1)
	return at, nil
}

// Delivered returns the tenant's total; DeliveredAll fills dst for every tenant (checker, under Census).
func (s *Sink) Delivered(idx int) int64 { return s.count[idx].Load() }

// DeliveredAll fills dst with every tenant's delivered count.
func (s *Sink) DeliveredAll(dst []int64) {
	for i := range dst {
		if i < len(s.count) {
			dst[i] = s.count[i].Load()
		}
	}
}

// Mismatches counts deliveries whose canary named another tenant (S2 evidence).
func (s *Sink) Mismatches() int64 { return s.mismatches.Load() }
