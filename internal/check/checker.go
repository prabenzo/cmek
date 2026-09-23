// Owner: Claude (verdict rules reviewed by Ben)
//
// Package check re-verifies the invariants from outside the service's trust boundary: the stored rows, the sink's
// record and the fake KMS's ground truth are evidence the service does not write. Once a second it reports one
// verdict per light to the metrics registry, which the page renders with no web change (DESIGN-REFERENCE › Data
// flow › (e)).
package check

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prabenzo/cmek/internal/kms"
	"github.com/prabenzo/cmek/internal/queue"
)

// Rows reads stored rows (queue.Store).
type Rows interface {
	Census(ctx context.Context, under func()) (queue.Census, error)
	CanaryFull(ctx context.Context, needle []byte) (int, error)
}

// Deliveries reads the sink's record (traffic.Sink).
type Deliveries interface {
	Delivered(idx int) int64
	DeliveredAll(dst []int64)
	DeliveredBetween(idx int, sinceSeq int64, lo, hi time.Time) (n, newSeq int64, overrun bool)
	Mismatches() int64
}

// Truth reads KMS ground truth (kms.Truth).
type Truth interface{ KeyEvents() []kms.KeyEvent }

// Health reads the service-side aggregates L1, L4 and the S3 display need (metrics.Registry).
type Health interface {
	HealthyP99() (current, baseline time.Duration, ok bool)
	HealthyRejections() int64
	OverloadedWithinShare() int64
	DeliveredPS() float64
	Scenario() (name string, running bool, since time.Time)
	DetectedRevokedAt(idx int) (at time.Time, purged int, ok bool)
}

// Reporter receives verdicts and checker timeline lines (metrics.Registry).
type Reporter interface {
	Report(id string, ok bool, count int64, at time.Time, detail string)
	Timeline(text string)
}

// Clock is the checker's time source.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Config wires the checker. L1 and L4 are judge flags: a light with its flag off is never reported, so its key
// is absent from the snapshot and the page shows it grey.
type Config struct {
	Tenants    []string
	Rows       Rows
	Deliveries Deliveries
	Truth      Truth
	Health     Health // may be nil when L1 and L4 are off (the S3 detection line then is not posted)
	Reporter   Reporter
	Clock      Clock

	Lease, Interval, FullScanEvery, L1Grace, L1For, L1Floor, L4Settle time.Duration
	CanaryPrefix                                                      string
	Capacity, L1Ratio, L4CapacityFactor                               float64
	GlobalCap, Workers, ClaimBatch                                    int
	L1, L4                                                            bool
}

// Checker re-verifies S1–S4, L1 and L4 once a second. Every counter is cumulative and never decreases: a violation
// stays red until the World is reset; L1 and L4 report this tick's verdict with n = their red ticks so far.
type Checker struct {
	cfg Config

	lastSeq                             []int64           // S3: the sink seq each tenant's windows resume from; −1 = unset
	seen                                map[int]time.Time // S3: idx → the tRevoke whose sinceSeq was initialised
	posted                              map[int]time.Time // S3: idx → the tRevoke whose detection line was posted
	sink                                []int64           // S4: the sink's totals sampled under the census
	lastScan                            time.Time         // S1: the last full scan
	s1Hits, s3Late, s4Bad, l1Red, l4Red int64
	s3Gap                               bool
	overSince                           time.Time    // L4: since when the backlog has exceeded Workers × ClaimBatch (zero = it does not)
	l1Since                             time.Time    // L1: since when the healthy p99 has been over its limit (zero = it is not)
	lastCensus                          queue.Census // L4 reads the census S4 just took
}

// New builds a checker; nothing runs until Run or Once.
func New(cfg Config) *Checker {
	n := len(cfg.Tenants)
	c := &Checker{cfg: cfg, lastSeq: make([]int64, n), seen: make(map[int]time.Time), posted: make(map[int]time.Time), sink: make([]int64, n)}
	for i := range c.lastSeq {
		c.lastSeq[i] = -1
	}
	return c
}

// Run calls Once every Interval until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.cfg.Clock.After(c.cfg.Interval):
			c.Once(ctx)
		}
	}
}

// Once runs one pass: S1 (every FullScanEvery), S2, S3, S4, then L1 and L4 when judged.
func (c *Checker) Once(ctx context.Context) {
	now := c.cfg.Clock.Now()
	c.s1(ctx, now)
	c.s2(now)
	c.s3(now)
	c.s4(ctx, now)
	if c.cfg.L1 {
		c.l1(now)
	}
	if c.cfg.L4 {
		c.l4(ctx, now)
	}
}

// s1: no plaintext at rest. One instr() scan of every stored ciphertext for the canary prefix, every FullScanEvery.
func (c *Checker) s1(ctx context.Context, now time.Time) {
	if !c.lastScan.IsZero() && now.Sub(c.lastScan) < c.cfg.FullScanEvery {
		return
	}
	c.lastScan = now
	hits, err := c.cfg.Rows.CanaryFull(ctx, []byte(c.cfg.CanaryPrefix))
	if err != nil {
		c.cfg.Reporter.Report("S1", c.s1Hits == 0, c.s1Hits, now, "scan failed: "+err.Error())
		return
	}
	c.s1Hits += int64(hits)
	detail := ""
	if c.s1Hits > 0 {
		detail = fmt.Sprintf("%d rows", c.s1Hits)
	}
	c.cfg.Reporter.Report("S1", c.s1Hits == 0, c.s1Hits, now, detail)
}

// s2: tenant key isolation. The sink counts every delivery whose payload named another tenant.
func (c *Checker) s2(now time.Time) {
	m := c.cfg.Deliveries.Mismatches()
	c.cfg.Reporter.Report("S2", m == 0, m, now, "")
}

// window is one revocation episode of one tenant from ground truth: deliveries in (revoke + Lease, restore] are
// violations; a zero restore means the key is still revoked (the window is open-ended).
type window struct {
	idx             int
	revoke, restore time.Time
}

// windows pairs each Enabled=false event with the next Enabled=true event of the same KEK, in event order.
func windows(events []kms.KeyEvent) []window {
	open := map[int]int{} // idx → index into out of the open window
	var out []window
	for _, e := range events {
		if !e.Enabled {
			if _, ok := open[e.Idx]; ok {
				continue // a repeated revoke inside an open episode changes nothing
			}
			open[e.Idx] = len(out)
			out = append(out, window{idx: e.Idx, revoke: e.At})
			continue
		}
		if i, ok := open[e.Idx]; ok {
			out[i].restore = e.At
			delete(open, e.Idx)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].idx < out[j].idx })
	return out
}

// s3: bounded revocation. For every episode from ground truth, count the sink's deliveries inside the violation
// window, resuming from the seq the tenant's last check reached. The first time a revoke is seen the resume point
// is the tenant's current total, which is at most one check late and at least Lease − Interval before the window
// opens, so no window delivery precedes it and a long-lived tenant never trips the ring's overrun by history alone.
func (c *Checker) s3(now time.Time) {
	ws := windows(c.cfg.Truth.KeyEvents())
	for i := 0; i < len(ws); {
		idx := ws[i].idx
		j := i
		for j < len(ws) && ws[j].idx == idx {
			j++
		}
		latest := ws[j-1]
		if c.seen[idx] != latest.revoke {
			c.seen[idx] = latest.revoke
			c.lastSeq[idx] = c.cfg.Deliveries.Delivered(idx)
		}
		since := c.lastSeq[idx]
		var newSeq int64 = since
		for _, w := range ws[i:j] {
			lo := w.revoke.Add(c.cfg.Lease)
			n, seq, overrun := c.cfg.Deliveries.DeliveredBetween(idx, since, lo, w.restore)
			c.s3Late += n
			if overrun {
				c.s3Gap = true
			}
			if seq > newSeq {
				newSeq = seq
			}
		}
		c.lastSeq[idx] = newSeq
		c.detectionLine(idx, latest.revoke)
		i = j
	}
	detail := ""
	if c.s3Gap {
		detail = "coverage gap"
	} else if c.s3Late > 0 {
		detail = fmt.Sprintf("%d decrypts after the bound", c.s3Late)
	}
	c.cfg.Reporter.Report("S3", c.s3Late == 0 && !c.s3Gap, c.s3Late, now, detail)
}

// detectionLine posts "t-0042 revoked at 12:00:59 (ground truth), detected in 8.2 s" once per episode, the second
// of the two lines per episode (the first is the service's own REVOKED audit line), and the only line that reads
// ground truth.
func (c *Checker) detectionLine(idx int, revoke time.Time) {
	if c.cfg.Health == nil || c.posted[idx] == revoke || idx < 0 || idx >= len(c.cfg.Tenants) {
		return
	}
	at, _, ok := c.cfg.Health.DetectedRevokedAt(idx)
	if !ok || !at.After(revoke) {
		return
	}
	c.posted[idx] = revoke
	c.cfg.Reporter.Timeline(fmt.Sprintf("%s revoked at %s (ground truth), detected in %.1f s", c.cfg.Tenants[idx], revoke.Format("15:04:05"), at.Sub(revoke).Seconds()))
}

// s4: no loss from key unavailability. One consistent census: per tenant accepted = delivered + ready + claimed +
// expired + dead, and the service's delivered is bounded by the sink's independent count sampled in the same hold.
func (c *Checker) s4(ctx context.Context, now time.Time) {
	census, err := c.cfg.Rows.Census(ctx, func() { c.cfg.Deliveries.DeliveredAll(c.sink) })
	if err != nil {
		c.cfg.Reporter.Report("S4", c.s4Bad == 0, c.s4Bad, now, "census failed: "+err.Error())
		return
	}
	detail := ""
	var bad int64
	for i := range census.Accepted {
		acc, del, exp, rdy, clm, dead := census.Accepted[i], census.Delivered[i], census.Expired[i], census.Ready[i], census.Claimed[i], census.Dead[i]
		sink := int64(0)
		if i < len(c.sink) {
			sink = c.sink[i]
		}
		off := acc - (del + rdy + clm + exp + dead)
		bound := del <= sink && sink <= del+clm
		if off != 0 || !bound {
			bad++
			if detail == "" {
				switch {
				case off != 0:
					detail = fmt.Sprintf("%s off by %d", c.name(i), off)
				default:
					detail = fmt.Sprintf("%s sink %d outside [%d, %d]", c.name(i), sink, del, del+clm)
				}
			}
		}
	}
	c.s4Bad += bad
	if detail == "" && c.s4Bad > 0 {
		detail = fmt.Sprintf("%d violations so far", c.s4Bad)
	}
	c.cfg.Reporter.Report("S4", c.s4Bad == 0, c.s4Bad, now, detail)
	c.lastCensus = census
}

func (c *Checker) name(i int) string {
	if i >= 0 && i < len(c.cfg.Tenants) {
		return c.cfg.Tenants[i]
	}
	return fmt.Sprintf("#%d", i)
}

// l1: blast radius. Judged while a scenario runs, after L1Grace (longer than the p99 window, so the scenario's
// opening burst has left the window): the healthy p99 stays within L1Ratio × max(baseline, L1Floor) and no
// unaffected tenant was rejected since the scenario started. A rejection is red at once; the p99 must stay over its
// limit for L1For before the light turns (a one-window blip, a checkpoint stall or a noisy host, stays a hover
// detail). Green "idle" with no scenario, and green "global surge" during one: every tenant is offered load then,
// so no tenant is unaffected and L1's premise does not hold (the healthy p99 tile and chart still show the rise;
// L4 is that scenario's light).
func (c *Checker) l1(now time.Time) {
	h := c.cfg.Health
	name, running, since := h.Scenario()
	if !running {
		c.l1Since = time.Time{}
		c.cfg.Reporter.Report("L1", true, c.l1Red, now, "idle")
		return
	}
	if global(name) {
		c.l1Since = time.Time{}
		c.cfg.Reporter.Report("L1", true, c.l1Red, now, "global surge")
		return
	}
	if now.Sub(since) < c.cfg.L1Grace {
		c.l1Since = time.Time{}
		c.cfg.Reporter.Report("L1", true, c.l1Red, now, "grace")
		return
	}
	cur, base, ok := h.HealthyP99()
	if !ok {
		c.cfg.Reporter.Report("L1", true, c.l1Red, now, "no baseline")
		return
	}
	limit := time.Duration(c.cfg.L1Ratio * float64(max(base, c.cfg.L1Floor)))
	rej := h.HealthyRejections()
	detail := ""
	switch {
	case rej > 0:
		detail = fmt.Sprintf("%d healthy rejections", rej)
	case cur > limit:
		if c.l1Since.IsZero() {
			c.l1Since = now
		}
		over := now.Sub(c.l1Since)
		if over < c.cfg.L1For {
			c.cfg.Reporter.Report("L1", true, c.l1Red, now, fmt.Sprintf("p99 %.0fms > %.0fms for %.0f of %.0f s", ms(cur), ms(limit), over.Seconds(), c.cfg.L1For.Seconds()))
			return
		}
		detail = fmt.Sprintf("p99 %.0fms > %.0fms for %.0f s", ms(cur), ms(limit), over.Seconds())
	default:
		c.l1Since = time.Time{}
	}
	red := detail != ""
	if red {
		c.l1Red++
	}
	c.cfg.Reporter.Report("L1", !red, c.l1Red, now, detail)
}

// l4: overload. Judged only while a global surge runs (its tail included) and the backlog has exceeded
// Workers × ClaimBatch for L4Settle: delivered stays at L4CapacityFactor × Capacity, the backlog stays under the
// cap plus the O(1) rule's overshoot bound, and no within-share tenant was shed. Green "idle" otherwise.
func (c *Checker) l4(ctx context.Context, now time.Time) {
	h := c.cfg.Health
	name, running, _ := h.Scenario()
	total, backlogged := c.lastCensus.Total, c.lastCensus.Backlogged
	if !running || !global(name) || total <= c.cfg.Workers*c.cfg.ClaimBatch {
		c.overSince = time.Time{}
		c.cfg.Reporter.Report("L4", true, c.l4Red, now, "idle")
		return
	}
	if c.overSince.IsZero() {
		c.overSince = now
	}
	if now.Sub(c.overSince) < c.cfg.L4Settle {
		c.cfg.Reporter.Report("L4", true, c.l4Red, now, "settling")
		return
	}
	del := h.DeliveredPS()
	want := c.cfg.L4CapacityFactor * c.cfg.Capacity
	bound := c.cfg.GlobalCap
	if backlogged > 0 {
		bound += backlogged * (c.cfg.GlobalCap / backlogged)
	}
	within := h.OverloadedWithinShare()
	detail := ""
	switch {
	case del < want:
		detail = fmt.Sprintf("delivered %.0f/s < %.0f", del, want)
	case total > bound:
		detail = fmt.Sprintf("backlog %d > %d", total, bound)
	case within > 0:
		detail = fmt.Sprintf("%d within-share tenants shed", within)
	}
	red := detail != ""
	if red {
		c.l4Red++
	}
	c.cfg.Reporter.Report("L4", !red, c.l4Red, now, detail)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// global reports whether the scenario offers load to every tenant (the two global surges): L4's scenarios, and the
// ones in which L1 is not judged.
func global(name string) bool { return name == "global_surge" || name == "no_cache_surge" }
