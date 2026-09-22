// Owner: Claude (reviewed by Ben)
package world

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *testClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// TestTwoWorlds: two Worlds from one Params run side by side in one process without sharing a counter or a file (no globals).
func TestTwoWorlds(t *testing.T) {
	dir := t.TempDir()
	clk := &testClock{now: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	mk := func(id string) *World {
		p := Small()
		p.DBDir = dir
		w, err := New(p, Deps{ID: id, Clock: clk})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		w.Start()
		return w
	}
	a, b := mk("a"), mk("b")
	send := func(w *World, n int) {
		for i := 0; i < n; i++ {
			tenant := fmt.Sprintf("t-%04d", i%w.P.Tenants)
			if err := w.Ingest(context.Background(), tenant, []byte(fmt.Sprintf(`{"canary":"%s%s","i":%d}`, w.P.CanaryPrefix, tenant, i))); err != nil {
				t.Fatalf("%s ingest %d: %v", w.ID, i, err)
			}
		}
	}
	send(a, 50)
	send(b, 30)
	delivered := func(w *World) int64 {
		dst := make([]int64, w.P.Tenants)
		w.sink.DeliveredAll(dst)
		var sum int64
		for _, v := range dst {
			sum += v
		}
		return sum
	}
	deadline := time.Now().Add(10 * time.Second)
	// The sink counts a delivery before the worker acks it, so wait for the acks (Total == 0) as well.
	settled := func() bool {
		return delivered(a) >= 50 && delivered(b) >= 30 && a.store.Total() == 0 && b.store.Total() == 0
	}
	for !settled() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for _, x := range []struct {
		w    *World
		want int64
	}{{a, 50}, {b, 30}} {
		if got := delivered(x.w); got != x.want {
			t.Errorf("%s: delivered %d, want %d", x.w.ID, got, x.want)
		}
		if x.w.store.Total() != 0 {
			t.Errorf("%s: total backlog %d, want 0", x.w.ID, x.w.store.Total())
		}
		for i := 0; i < x.w.P.Tenants; i++ {
			if x.w.store.Backlog(i) != 0 {
				t.Errorf("%s: tenant %d backlog %d", x.w.ID, i, x.w.store.Backlog(i))
			}
			if x.w.sink.Delivered(i) < 1 { // every tenant sent at least once: a Next that starves a class cannot pass on other tenants' deliveries
				t.Errorf("%s: tenant %d delivered nothing", x.w.ID, i)
			}
		}
		if x.w.sink.Mismatches() != 0 {
			t.Errorf("%s: %d canary mismatches", x.w.ID, x.w.sink.Mismatches())
		}
	}
	for _, w := range []*World{a, b} {
		start := time.Now()
		w.Stop()
		if d := time.Since(start); d > w.P.StopTimeout+100*time.Millisecond {
			t.Errorf("%s: Stop took %v", w.ID, d)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			p := filepath.Join(dir, fmt.Sprintf("killswitch-%d-%s.db%s", os.Getpid(), w.ID, suffix))
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s: %s still exists after Stop", w.ID, p)
			}
		}
	}
}

// TestSmallPopulation: New must accept fewer than five tenants (the ranks log line used to index rank 5).
func TestSmallPopulation(t *testing.T) {
	p := Small()
	p.Tenants = 2
	p.DBDir = t.TempDir()
	w, err := New(p, Deps{ID: "tiny", Clock: &testClock{now: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	w.Stop()
}
