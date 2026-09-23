// Owner: Claude (reviewed by Ben)
package metrics

import (
	"math"
	"sync/atomic"
	"time"
)

// histBuckets is the fixed bucket count: bucket b covers [1ms·1.5^b, 1ms·1.5^(b+1)); b = 27 is the 60 s cap.
const histBuckets = 28

// hist is one tick's latency histogram for one class, written lock-free by Delivered.
type hist [histBuckets]atomic.Uint32

// bucket maps a latency to its index: clamp(floor(log(d/1ms)/ln 1.5), 0, 27). ln15 is a Registry field set in
// New (no package-level var: CI greps '^var ').
func (r *Registry) bucket(d time.Duration) int {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 1 {
		return 0
	}
	b := int(math.Floor(math.Log(ms) / r.ln15))
	if b < 0 {
		return 0
	}
	if b >= histBuckets {
		return histBuckets - 1
	}
	return b
}

// bucketLo is the lower edge of bucket b.
func bucketLo(b int) time.Duration {
	return time.Duration(math.Pow(1.5, float64(b)) * float64(time.Millisecond))
}

// p99 sums the last P99Window ticks for one class (0 healthy, 1 affected): n samples, 0 when n == 0; rank =
// floor(0.99·n)+1 (the first sample of the slowest 1 %); the first bucket whose cumulative count ≥ rank; then
// lo + (hi−lo)·(rank − cumBefore − ½)/inBucket: the k-th of the bucket's samples sits at its (k − ½)/in position
// under a uniform spread, so the result stays inside [lo, hi) (M4 Decision: an upper-edge read would step 1.5×
// across an edge and trip the 1.25 L1 ratio by itself).
func (r *Registry) p99(class int) time.Duration {
	var sum [histBuckets]uint64
	var n uint64
	for i := range r.hists[class] {
		for b := range sum {
			c := uint64(r.hists[class][i][b].Load())
			sum[b] += c
			n += c
		}
	}
	if n == 0 {
		return 0
	}
	rank := uint64(math.Floor(0.99*float64(n))) + 1
	var cum uint64
	for b, in := range sum {
		if cum+in >= rank {
			lo, hi := bucketLo(b), bucketLo(b+1)
			return lo + time.Duration(float64(hi-lo)*(float64(rank-cum)-0.5)/float64(in))
		}
		cum += in
	}
	return bucketLo(histBuckets) // unreachable: rank ≤ n
}

// record adds one sample to the tick slot being written; lock-free (Delivered).
func (r *Registry) record(class int, d time.Duration) {
	r.hists[class][r.slot.Load()][r.bucket(d)].Add(1)
}

// roll reads both windowed p99s, then opens the next tick slot: the slot P99Window ticks old is zeroed before
// the pointer moves, so a writer that still holds the old slot lands one tick late at worst. Called by tick.
func (r *Registry) roll() (healthy, affected time.Duration) {
	healthy, affected = r.p99(0), r.p99(1)
	next := (int(r.slot.Load()) + 1) % len(r.hists[0])
	for c := range r.hists {
		for b := range r.hists[c][next] {
			r.hists[c][next][b].Store(0)
		}
	}
	r.slot.Store(int32(next))
	return healthy, affected
}
