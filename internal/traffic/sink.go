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

// Sink stamps deliveredAt on entry, verifies the canary's tenant, sleeps 5-20 ms, and records the delivery: the
// per-tenant total and, in a ring of Ring entries, each delivery's deliveredAt (S3's evidence).
type Sink struct {
	cfg        SinkConfig
	count      []atomic.Int64
	mismatches atomic.Int64
	mu         []sync.Mutex // per tenant: the ring and its count move together
	ring       [][]int64    // per tenant: deliveredAt unix-nanos, slot (seq-1) % Ring
}

// NewSink builds the sink.
func NewSink(cfg SinkConfig) *Sink {
	if cfg.Ring <= 0 {
		cfg.Ring = 1024
	}
	n := len(cfg.Tenants)
	s := &Sink{cfg: cfg, count: make([]atomic.Int64, n), mu: make([]sync.Mutex, n), ring: make([][]int64, n)}
	for i := range s.ring {
		s.ring[i] = make([]int64, cfg.Ring)
	}
	return s
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
	// S2 is evidence of cross-tenant delivery, not of payload shape: a payload that carries a canary must carry this
	// tenant's; a payload without one (a manual curl) is delivered like any other.
	if i := bytes.Index(plaintext, []byte(`"`+s.cfg.CanaryPrefix)); i >= 0 && !bytes.HasPrefix(plaintext[i:], []byte(`"`+s.cfg.CanaryPrefix+tenant+`"`)) {
		s.mismatches.Add(1)
		return at, ErrTenantMismatch
	}
	select {
	case <-s.cfg.Clock.After(s.latency()):
	case <-ctx.Done():
		return at, ctx.Err()
	}
	s.mu[idx].Lock()
	seq := s.count[idx].Load() + 1
	s.ring[idx][(seq-1)%int64(len(s.ring[idx]))] = at.UnixNano()
	s.count[idx].Store(seq)
	s.mu[idx].Unlock()
	return at, nil
}

// DeliveredBetween counts the tenant's deliveries with seq > sinceSeq and lo < deliveredAt ≤ hi (a zero hi is
// +∞), returns the tenant's current seq to resume from, and overrun when deliveries since sinceSeq have already
// left the ring (count − sinceSeq > Ring): the checker then cannot vouch for the window (S3 "coverage gap").
func (s *Sink) DeliveredBetween(idx int, sinceSeq int64, lo, hi time.Time) (n, newSeq int64, overrun bool) {
	if idx < 0 || idx >= len(s.ring) {
		return 0, sinceSeq, false
	}
	s.mu[idx].Lock()
	defer s.mu[idx].Unlock()
	total := s.count[idx].Load()
	ring := s.ring[idx]
	size := int64(len(ring))
	if sinceSeq < 0 {
		sinceSeq = 0
	}
	if total-sinceSeq > size {
		overrun = true
		sinceSeq = total - size
	}
	loN, hiN := lo.UnixNano(), int64(0)
	if !hi.IsZero() {
		hiN = hi.UnixNano()
	}
	for seq := sinceSeq + 1; seq <= total; seq++ {
		at := ring[(seq-1)%size]
		if at > loN && (hiN == 0 || at <= hiN) {
			n++
		}
	}
	return n, total, overrun
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
