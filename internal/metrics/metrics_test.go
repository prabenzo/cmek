// Owner: Claude (reviewed by Ben)
package metrics

import (
	"math"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time                         { return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type fakeGrid struct{ states []uint8 }

func (g *fakeGrid) States(dst []uint8) { copy(dst, g.states) }

type fakeBacklog struct{ per []int }

func (b *fakeBacklog) Backlog(idx int) int { return b.per[idx] }
func (b *fakeBacklog) Total() int {
	t := 0
	for _, v := range b.per {
		t += v
	}
	return t
}

func newTestRegistry(n int) (*Registry, *fakeClock, *fakeGrid, *fakeBacklog) {
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "t-" + string(rune('a'+i))
	}
	g := &fakeGrid{states: make([]uint8, n)}
	b := &fakeBacklog{per: make([]int, n)}
	r := New(Config{Tenants: ids, Providers: []string{"aws", "gcp", "azure"}, Grid: g, Backlog: b, Clock: clk, Interval: 500 * time.Millisecond, P99Window: 10, BaselineTicks: 20, AggregateMin: 3, DrainSlack: 4})
	return r, clk, g, b
}

// TestHistP99: bucket edges, the rank rule, the empty and single-sample cases, in-bucket interpolation across a
// 10 % slowdown, the 10-tick window, and the frozen baseline.
func TestHistP99(t *testing.T) {
	r, clk, _, _ := newTestRegistry(6)
	for _, row := range []struct {
		d    time.Duration
		want int
	}{{time.Millisecond, 0}, {1499 * time.Microsecond, 0}, {1500 * time.Microsecond, 1}, {60 * time.Second, 27}, {10 * time.Minute, 27}, {0, 0}} {
		if got := r.bucket(row.d); got != row.want {
			t.Errorf("bucket(%v) = %d, want %d", row.d, got, row.want)
		}
	}
	inBucket := func(d time.Duration, b int) bool { return d >= bucketLo(b) && d < bucketLo(b+1) }
	if got := r.p99(0); got != 0 {
		t.Errorf("empty p99 = %v, want 0", got)
	}
	// rank rule: 99 × 20 ms + 1 × 500 ms → the 100th sample, in the 500 ms bucket [437, 656) ms
	for i := 0; i < 99; i++ {
		r.Delivered(0, 20*time.Millisecond)
	}
	r.Delivered(0, 500*time.Millisecond)
	if got := r.p99(0); !inBucket(got, r.bucket(500*time.Millisecond)) {
		t.Errorf("99×20ms+500ms p99 = %v, want in [437, 656) ms", got)
	}
	// 100 × 20 ms → in 20 ms's bucket [17.1, 25.6) ms
	r, clk, _, _ = newTestRegistry(6)
	for i := 0; i < 100; i++ {
		r.Delivered(0, 20*time.Millisecond)
	}
	if got := r.p99(0); !inBucket(got, r.bucket(20*time.Millisecond)) {
		t.Errorf("100×20ms p99 = %v, want in [17.1, 25.6) ms", got)
	}
	// n = 1 → that sample's bucket
	r, clk, _, _ = newTestRegistry(6)
	r.Delivered(0, 3*time.Second)
	if got := r.p99(0); !inBucket(got, r.bucket(3*time.Second)) {
		t.Errorf("single p99 = %v, want in 3 s bucket", got)
	}
	// interpolation: 1,000 samples spread evenly over 10–30 ms, then the same 10 % slower → ratio < 1.25 (an
	// upper-edge read would jump 1.5× when the tail crosses a bucket edge)
	r, clk, _, _ = newTestRegistry(6)
	for i := 0; i < 1000; i++ {
		r.Delivered(0, 10*time.Millisecond+time.Duration(i)*20*time.Microsecond)
	}
	base := r.p99(0)
	r, clk, _, _ = newTestRegistry(6)
	for i := 0; i < 1000; i++ {
		r.Delivered(0, time.Duration(1.1*float64(10*time.Millisecond+time.Duration(i)*20*time.Microsecond)))
	}
	if slower := r.p99(0); float64(slower)/float64(base) >= 1.25 || slower <= base {
		t.Errorf("10%% slower: %v vs %v (ratio %.3f), want 1 < ratio < 1.25", slower, base, float64(slower)/float64(base))
	}
	// window: samples only before tick 1 count through tick 10 and are gone at tick 11; the healthy ring feeds the
	// baseline, frozen by SetTargets and untouched by later samples
	r, clk, _, _ = newTestRegistry(6)
	for i := 0; i < 50; i++ {
		r.Delivered(0, 40*time.Millisecond)
	}
	want := r.p99(0)
	for tick := 1; tick <= 11; tick++ {
		clk.now = clk.now.Add(500 * time.Millisecond)
		r.tick()
		r.mu.Lock()
		last := r.p99Ring[(r.p99Head-1+len(r.p99Ring))%len(r.p99Ring)]
		r.mu.Unlock()
		if tick <= 10 && last != want {
			t.Errorf("tick %d: windowed p99 %v, want %v", tick, last, want)
		}
		if tick == 11 && last != 0 {
			t.Errorf("tick 11: windowed p99 %v, want 0 (window passed)", last)
		}
	}
	r.SetTargets([]int{1, 2})
	r.mu.Lock()
	baseline := r.baseline
	r.mu.Unlock()
	if wantMean := time.Duration(float64(want) * 10 / 11); math.Abs(float64(baseline-wantMean)) > float64(time.Microsecond) {
		t.Errorf("baseline %v, want mean of the ring %v", baseline, wantMean)
	}
	if !r.Affected(1) || r.Affected(0) {
		t.Error("affected bits: want 1 set, 0 clear")
	}
	for i := 0; i < 50; i++ {
		r.Delivered(0, 900*time.Millisecond)
	}
	clk.now = clk.now.Add(500 * time.Millisecond)
	r.tick()
	r.mu.Lock()
	after := r.baseline
	r.mu.Unlock()
	if after != baseline {
		t.Errorf("baseline moved after SetTargets: %v → %v", baseline, after)
	}
	r.ClearTargets()
	if r.Affected(1) {
		t.Error("affected bit set after ClearTargets")
	}
}

// TestTransitionLines pins the timeline rule for state changes: a REVOKED transition posts at once with its purge
// count; AggregateMin or more identical (provider, from, to) transitions collapse to one line at the next flush
// while fewer keep the M2 single form; ACTIVE↔RIDING_THROUGH churn flushes once per second as one line per
// provider; nothing flushes twice within a second; every line a flush posts carries the flush instant.
func TestTransitionLines(t *testing.T) {
	r, clk, _, _ := newTestRegistry(9) // three tenants per provider: a-c aws, d-f gcp, g-i azure
	lines := func(n int) []event {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.events(n)
	}
	flush := func(at time.Time) {
		r.mu.Lock()
		r.flushTransitions(at)
		r.mu.Unlock()
	}
	t0 := clk.now
	state := func(idx int, from, to uint8, detail string, purged int) {
		r.Audit(Audit{At: t0.Add(100 * time.Millisecond), Idx: idx, Op: "state", From: from, To: to, Detail: detail, Purged: purged})
	}
	// REVOKED posts at once, before any flush
	state(0, 0, 3, "key revoked, 2 DEKs purged", 2)
	if got := lines(1); len(got) != 1 || got[0].Text != "t-a REVOKED, 2 DEKs purged" || got[0].At != t0.Add(100*time.Millisecond).UnixMilli() {
		t.Fatalf("revoked line = %+v", got)
	}
	// three gcp cold fetches aggregate; the one azure single keeps its form; churn on aws counts per provider
	for _, idx := range []int{3, 4, 5, 6} {
		state(idx, 0, 2, "cold fetch failed", 0)
	}
	state(1, 0, 1, "renewal failed", 0)
	state(2, 0, 1, "renewal failed", 0)
	state(1, 1, 0, "authorization renewed", 0)
	if got := lines(9); len(got) != 1 {
		t.Fatalf("buffered transitions posted before the flush: %+v", got)
	}
	flush(t0.Add(time.Second))
	got := lines(9)
	want := []string{"aws: 2 ACTIVE → RIDING_THROUGH, 1 back", "t-g ACTIVE → KEY_UNAVAILABLE (cold fetch failed)", "3 gcp tenants ACTIVE → KEY_UNAVAILABLE", "t-a REVOKED, 2 DEKs purged"}
	if len(got) != len(want) {
		t.Fatalf("lines after flush = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("line %d = %q, want %q", i, got[i].Text, w)
		}
		if i < 3 && got[i].At != t0.Add(time.Second).UnixMilli() {
			t.Errorf("line %d at %d, want the flush instant %d", i, got[i].At, t0.Add(time.Second).UnixMilli())
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq >= got[i-1].Seq || got[i].At > got[i-1].At {
			t.Errorf("lines out of order: %+v then %+v", got[i], got[i-1])
		}
	}
	// a second flush within the second posts nothing; the next second flushes the new churn as one line
	state(7, 0, 1, "renewal failed", 0)
	flush(t0.Add(1500 * time.Millisecond))
	if got := lines(1); got[0].Text != want[0] {
		t.Errorf("flushed within a second: %q", got[0].Text)
	}
	flush(t0.Add(2 * time.Second))
	if got := lines(1); got[0].Text != "azure: 1 ACTIVE → RIDING_THROUGH, 0 back" || got[0].At != t0.Add(2*time.Second).UnixMilli() {
		t.Errorf("second flush = %+v", got)
	}
}

// TestProviderCallsPS: KMS calls are counted per provider per tick for the no-cache summary lines.
func TestProviderCallsPS(t *testing.T) {
	r, clk, _, _ := newTestRegistry(6) // a,b aws · c,d gcp · e,f azure
	for _, idx := range []int{0, 1, 0} {
		r.Audit(Audit{At: clk.now, Idx: idx, Op: "unwrap", Outcome: "ok"})
	}
	r.Audit(Audit{At: clk.now, Idx: 2, Op: "generate", Outcome: "ok"})
	r.Audit(Audit{At: clk.now, Idx: 4, Op: "state", Outcome: "x"}) // not a call
	r.tick()
	for _, row := range []struct {
		prov string
		want float64
	}{{"aws", 6}, {"gcp", 2}, {"azure", 0}} {
		if got := r.ProviderCallsPS(row.prov); got != row.want {
			t.Errorf("%s: %v calls/s, want %v", row.prov, got, row.want)
		}
	}
	if last := r.Last(); last.ProviderCallsPS["aws"] != 6 || last.KMSCallsPS != 8 {
		t.Errorf("Last = %+v", last)
	}
	r.tick()
	if got := r.ProviderCallsPS("aws"); got != 0 {
		t.Errorf("counter not reset per tick: %v", got)
	}
}
