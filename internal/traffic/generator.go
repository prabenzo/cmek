// Owner: Claude
package traffic

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Ingester is the front door; world.World implements it and the HTTP handler calls the same method.
type Ingester interface {
	Ingest(ctx context.Context, tenant string, payload []byte) error
}

// Clock is the generator's and sink's time source.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// ZipfRates returns rates by rank (index 0 = rank 1) summing to total, rate_k proportional to k^-s.
func ZipfRates(n int, total, s float64) []float64 {
	rates := make([]float64, n)
	sum := 0.0
	for k := 1; k <= n; k++ {
		rates[k-1] = math.Pow(float64(k), -s)
		sum += rates[k-1]
	}
	for i := range rates {
		rates[i] = rates[i] / sum * total
	}
	return rates
}

// Payload builds a ~size-byte synthetic webhook event containing the canary prefix+tenant.
func Payload(tenant string, seq int64, size int, prefix string) []byte {
	head := fmt.Sprintf(`{"tenant":"%s","seq":%d,"canary":"%s%s","event":"order.created","pad":"`, tenant, seq, prefix, tenant)
	pad := size - len(head) - 2
	if pad < 0 {
		pad = 0
	}
	return []byte(head + strings.Repeat("x", pad) + `"}`)
}

// GeneratorConfig sizes the load generator; Rates is by grid index.
type GeneratorConfig struct {
	Tenants      []string
	Rates        []float64
	PayloadBytes int
	CanaryPrefix string
	Ingest       Ingester
	Clock        Clock
	Rand         *rand.Rand
	Lock         *sync.Mutex
}

// Generator runs one goroutine per tenant with exponential inter-arrivals; it starts paused.
type Generator struct {
	cfg     GeneratorConfig
	mu      sync.Mutex
	running chan struct{} // closed while running
	on      bool
	mult    []float64
	global  float64
	seq     []int64
}

// NewGenerator builds the generator, paused until SetRunning(true).
func NewGenerator(cfg GeneratorConfig) *Generator {
	g := &Generator{cfg: cfg, running: make(chan struct{}), mult: make([]float64, len(cfg.Tenants)), global: 1, seq: make([]int64, len(cfg.Tenants))}
	for i := range g.mult {
		g.mult[i] = 1
	}
	return g
}

func (g *Generator) runningCh() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running
}

func (g *Generator) rate(idx int) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cfg.Rates[idx] * g.mult[idx] * g.global
}

func (g *Generator) exp() float64 {
	g.cfg.Lock.Lock()
	defer g.cfg.Lock.Unlock()
	return g.cfg.Rand.ExpFloat64()
}

func (g *Generator) next(idx int) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq[idx]++
	return g.seq[idx]
}

// Run starts every tenant loop and returns when ctx ends.
func (g *Generator) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := range g.cfg.Tenants {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			g.loop(ctx, idx)
		}(i)
	}
	wg.Wait()
}

func (g *Generator) loop(ctx context.Context, idx int) {
	tenant := g.cfg.Tenants[idx]
	for {
		select {
		case <-g.runningCh():
		case <-ctx.Done():
			return
		}
		r := g.rate(idx)
		var wait time.Duration
		if r <= 0 {
			wait = time.Second
		} else {
			wait = time.Duration(g.exp() / r * float64(time.Second))
		}
		select {
		case <-g.cfg.Clock.After(wait):
		case <-ctx.Done():
			return
		}
		if r <= 0 {
			continue
		}
		g.mu.Lock()
		on := g.on
		g.mu.Unlock()
		if !on {
			continue // paused during the wait: do not send after resume
		}
		_ = g.cfg.Ingest.Ingest(ctx, tenant, Payload(tenant, g.next(idx), g.cfg.PayloadBytes, g.cfg.CanaryPrefix))
	}
}

// SetRunning pauses or resumes all loops (viewer count 0 <-> >0).
func (g *Generator) SetRunning(on bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if on == g.on {
		return
	}
	g.on = on
	if on {
		close(g.running)
	} else {
		g.running = make(chan struct{})
	}
}

// SetMultiplier scales one tenant; SetGlobal scales everyone; 1 restores.
func (g *Generator) SetMultiplier(idx int, x float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mult[idx] = x
}

// SetGlobal scales every tenant's rate.
func (g *Generator) SetGlobal(x float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.global = x
}

// OfferedPS is the current total offered rate (tile); Offered(idx) one tenant's current rate.
func (g *Generator) OfferedPS() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	sum := 0.0
	for i, r := range g.cfg.Rates {
		sum += r * g.mult[i]
	}
	return sum * g.global
}

// Offered returns one tenant's current offered rate.
func (g *Generator) Offered(idx int) float64 { return g.rate(idx) }
