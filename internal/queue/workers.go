// Owner: Claude
package queue

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prabenzo/cmek/internal/cmek"
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
}

// Workers is the fixed pool; Run blocks until ctx ends.
type Workers struct{ cfg WorkersConfig }

// NewWorkers builds the pool.
func NewWorkers(cfg WorkersConfig) *Workers { return &Workers{cfg: cfg} }

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
			select {
			case <-c.Store.Wake():
			case <-c.Clock.After(c.IdlePoll):
			case <-ctx.Done():
				return
			}
			continue
		}
		batch, err := c.Store.Claim(ctx, idx, c.ClaimBatch, c.Clock.Now().Add(c.ClaimTimeout))
		if err != nil || len(batch.Msgs) == 0 {
			continue
		}
		var acked []int64
		for i, m := range batch.Msgs {
			h, err := c.Keys.DecryptKey(batch.Tenant, m.DEKID)
			if err != nil {
				if errors.Is(err, cmek.ErrPoison) {
					_ = c.Store.Dead(ctx, idx, m.ID)
					continue
				}
				if errors.Is(err, cmek.ErrDEKCold) {
					c.Keys.Warm(batch.Tenant, m.DEKID)
				}
				_, _ = c.Store.Release(ctx, idx, ids(batch.Msgs[i:]))
				break
			}
			pt, err := cmek.Open(h, c.Clock.Now(), batch.Tenant, m.ID, cmek.Envelope{DEKID: m.DEKID, Nonce: m.Nonce, Ciphertext: m.Ciphertext})
			h.Zero()
			if err != nil {
				if errors.Is(err, cmek.ErrPoison) {
					_ = c.Store.Dead(ctx, idx, m.ID)
					continue
				}
				_, _ = c.Store.Release(ctx, idx, ids(batch.Msgs[i:]))
				break
			}
			deliveredAt, err := c.Sink.Deliver(ctx, batch.Tenant, idx, m.ID, pt)
			if err != nil {
				if ctx.Err() != nil {
					_, _ = c.Store.Release(ctx, idx, ids(batch.Msgs[i:]))
					break
				}
				_ = c.Store.Dead(ctx, idx, m.ID) // the sink saw another tenant's canary: S2 witness
				continue
			}
			c.Recorder.Delivered(idx, deliveredAt.Sub(m.EnqueuedAt))
			acked = append(acked, m.ID)
		}
		if len(acked) > 0 {
			_, _ = c.Store.Ack(ctx, idx, acked)
		}
	}
}
