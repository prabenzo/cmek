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
