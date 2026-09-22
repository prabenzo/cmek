// Owner: Claude
package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prabenzo/cmek/internal/cmek"
	"github.com/prabenzo/cmek/internal/logx"
)

// Keys is what workers need from the core.
type Keys interface {
	DecryptKey(tenant, dekID string) (cmek.Handle, error)
	Warm(tenant, dekID string)
}

// Sink receives plaintext deliveries (traffic.Sink); returns the deliveredAt stamp.
type Sink interface {
	Deliver(ctx context.Context, tenant string, idx int, msgID int64, plaintext []byte) (time.Time, error)
}

// Recorder receives end-to-end latencies (metrics.Registry).
type Recorder interface {
	Delivered(idx int, latency time.Duration)
}

// WorkerClock is the workers' time source.
type WorkerClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// WorkersConfig wires the pool.
type WorkersConfig struct {
	Store        *Store
	Sched        *Scheduler
	Keys         Keys
	Sink         Sink
	Recorder     Recorder
	Clock        WorkerClock
	Tenants      []string
	Workers      int
	ClaimBatch   int
	ClaimTimeout time.Duration
	IdlePoll     time.Duration
	Logger       *slog.Logger // optional; every error branch logs, throttled to one line per second per message
}

// Workers is the fixed pool; Run blocks until ctx ends.
type Workers struct {
	cfg WorkersConfig
	log logx.Throttle
}

// NewWorkers builds the pool.
func NewWorkers(cfg WorkersConfig) *Workers {
	return &Workers{cfg: cfg, log: logx.Throttle{Log: cfg.Logger}}
}

// wait blocks until a row is inserted or released, IdlePoll elapses, or ctx ends; false means ctx ended.
func (w *Workers) wait(ctx context.Context) bool {
	select {
	case <-w.cfg.Store.Wake():
	case <-w.cfg.Clock.After(w.cfg.IdlePoll):
	case <-ctx.Done():
		return false
	}
	return true
}

// dead dead-letters one row and logs why (poison at decrypt, or the sink refused it).
func (w *Workers) dead(ctx context.Context, b Batch, id int64, why string, cause error) {
	w.log.Error("message dead-lettered", "why", why, "tenant", b.Tenant, "id", id, "err", cause)
	if err := w.cfg.Store.Dead(ctx, b.Idx, id); err != nil {
		w.log.Error("dead-letter failed", "tenant", b.Tenant, "id", id, "err", err)
	}
}

// release returns the unprocessed rest of a batch untouched (S4) and logs the reason.
func (w *Workers) release(ctx context.Context, b Batch, rest []Message, why string, cause error) {
	if ctx.Err() == nil { // a stopping worker releases silently
		w.log.Error("batch released", "why", why, "tenant", b.Tenant, "rows", len(rest), "err", cause)
	}
	if _, err := w.cfg.Store.Release(ctx, b.Idx, ids(rest)); err != nil && ctx.Err() == nil {
		w.log.Error("release failed", "tenant", b.Tenant, "rows", len(rest), "err", err)
	}
}

// Run starts cfg.Workers goroutines and returns when every one has stopped.
func (w *Workers) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Wait()
}

func ids(msgs []Message) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

// loop: pick a hot tenant with ready rows, claim a batch, decrypt each message with a cached DEK, deliver, ack.
// A missing or unusable key releases the remaining rows untouched (S4); a poison row is dead-lettered alone; workers never call a KMS.
func (w *Workers) loop(ctx context.Context) {
	c := w.cfg
	for ctx.Err() == nil {
		idx, ok := c.Sched.Next()
		if !ok {
			if !w.wait(ctx) {
				return
			}
			continue
		}
		batch, err := c.Store.Claim(ctx, idx, c.ClaimBatch, c.Clock.Now().Add(c.ClaimTimeout))
		if err != nil || len(batch.Msgs) == 0 {
			// A persistent Claim error (busy past busy_timeout, I/O, a closed store) must not hot-spin the pool:
			// wait as the idle branch does. An empty batch means the ledger and the table disagree or another
			// worker took the rows; same wait.
			if err != nil && ctx.Err() == nil {
				w.log.Error("claim failed", "tenant", c.Tenants[idx], "err", err)
			}
			if !w.wait(ctx) {
				return
			}
			continue
		}
		var acked []int64
		for i, m := range batch.Msgs {
			h, err := c.Keys.DecryptKey(batch.Tenant, m.DEKID)
			if err != nil {
				if errors.Is(err, cmek.ErrPoison) {
					w.dead(ctx, batch, m.ID, "foreign dek id", err)
					continue
				}
				if errors.Is(err, cmek.ErrDEKCold) {
					c.Keys.Warm(batch.Tenant, m.DEKID)
				}
				w.release(ctx, batch, batch.Msgs[i:], "key not usable", err)
				break
			}
			pt, err := cmek.Open(h, c.Clock.Now(), batch.Tenant, m.ID, cmek.Envelope{DEKID: m.DEKID, Nonce: m.Nonce, Ciphertext: m.Ciphertext})
			h.Zero()
			if err != nil {
				if errors.Is(err, cmek.ErrPoison) {
					w.dead(ctx, batch, m.ID, "envelope does not open", err)
					continue
				}
				w.release(ctx, batch, batch.Msgs[i:], "lease lapsed at decrypt", err)
				break
			}
			deliveredAt, err := c.Sink.Deliver(ctx, batch.Tenant, idx, m.ID, pt)
			if err != nil {
				if ctx.Err() != nil {
					w.release(ctx, batch, batch.Msgs[i:], "stopping", err)
					break
				}
				w.dead(ctx, batch, m.ID, "sink refused delivery", err) // the sink saw another tenant's canary: S2 witness
				continue
			}
			c.Recorder.Delivered(idx, deliveredAt.Sub(m.EnqueuedAt))
			acked = append(acked, m.ID)
		}
		if len(acked) > 0 {
			if _, err := c.Store.Ack(ctx, idx, acked); err != nil && ctx.Err() == nil {
				w.log.Error("ack failed", "tenant", batch.Tenant, "rows", len(acked), "err", err)
			}
		}
	}
}
