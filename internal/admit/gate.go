// Owner: Claude
// Package admit is the admission gate: a global backlog cap, a per-tenant token bucket and a per-tenant backlog
// cap, checked cheapest first before any key work, from lock-free ledger reads. A rejected event costs no KMS
// call and no data-key message. FairShare = false is the M3 cut line: a plain global cap that sheds everyone.
package admit

import (
	"errors"
	"math"
	"time"

	"golang.org/x/time/rate"
)

var ErrRateLimited = errors.New("admit: rate_limited") // 429, Retry-After RateLimited
var ErrBacklogFull = errors.New("admit: backlog_full") // 429, Retry-After BacklogFull
var ErrOverloaded = errors.New("admit: overloaded")    // 503, Retry-After Overloaded

// Backlog is the ledger view admission needs (queue.Store): three atomics, no lock.
type Backlog interface {
	Backlog(idx int) int
	Total() int
	Backlogged() int
}

// Clock feeds x/time/rate's AllowN.
type Clock interface{ Now() time.Time }

// Config sizes the gate; FairShare = false is the plain global cap.
type Config struct {
	Tenants                     int
	Rate                        float64 // events/s per tenant
	Burst, TenantCap, GlobalCap int
	FairShare                   bool
	Backlog                     Backlog
	Clock                       Clock
}

// Gate applies global cap, token bucket and tenant cap in the spec's order.
type Gate struct {
	limiters []*rate.Limiter
	cfg      Config
}

// New builds one limiter per tenant (≈ 100 µs for 1,000).
func New(cfg Config) *Gate {
	g := &Gate{cfg: cfg, limiters: make([]*rate.Limiter, cfg.Tenants)}
	for i := range g.limiters {
		g.limiters[i] = rate.NewLimiter(rate.Limit(cfg.Rate), cfg.Burst)
	}
	return g
}

// Admit returns nil or one sentinel, plus the share class of the ONE Backlog(idx) read it decided on, so the L4
// counter is falsifiable: the shed decision and the within/over classification can never disagree. Check order:
// global cap → token bucket → tenant backlog cap; a token consumed by a request that then fails on the key is
// not refunded. Under the cut (FairShare = false) the cap sheds everyone, classed within share by design.
func (g *Gate) Admit(idx int) (within bool, err error) {
	b := g.cfg.Backlog
	bl := b.Backlog(idx)
	within = bl <= g.FairShare()
	if b.Total() >= g.cfg.GlobalCap && (!g.cfg.FairShare || !within) {
		return within, ErrOverloaded
	}
	if !g.limiters[idx].AllowN(g.cfg.Clock.Now(), 1) {
		return within, ErrRateLimited
	}
	if bl >= g.cfg.TenantCap {
		return within, ErrBacklogFull
	}
	return within, nil
}

// FairShare is GlobalCap / max(1, Backlogged) when fair share is on, else math.MaxInt (every tenant is within
// share); two atomics, no lock, so the scheduler may call it under its own mutex.
func (g *Gate) FairShare() int {
	if !g.cfg.FairShare {
		return math.MaxInt
	}
	n := g.cfg.Backlog.Backlogged()
	if n < 1 {
		n = 1
	}
	return g.cfg.GlobalCap / n
}

// WithinShare reports Backlog(idx) ≤ FairShare() for callers outside Ingest (Ingest uses Admit's own read).
func (g *Gate) WithinShare(idx int) bool { return g.cfg.Backlog.Backlog(idx) <= g.FairShare() }
