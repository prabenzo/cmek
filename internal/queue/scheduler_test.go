// Owner: Claude (reviewed by Ben: Next)
package queue

import (
	"context"
	"testing"
)

type alwaysHot struct{}

func (alwaysHot) Hot(string) bool { return true }

type neverHot struct{}

func (neverHot) Hot(string) bool { return false }

type fixedShare struct{ fs int }

func (f fixedShare) FairShare() int { return f.fs }

// TestTwoClassNext: LightTurns light turns then one heavy turn, independent cursors, work-conserving fall-through,
// an unchanged state on a double miss, and the M1 ring when the pass is off.
func TestTwoClassNext(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := &testClock{}
	s, err := Open(Config{Path: dir + "/s.db", Tenants: []string{"t-0000", "t-0001", "t-0002"}, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows := map[int]int{0: 5, 1: 1, 2: 1} // tenant 0 is heavy (backlog 5), tenants 1 and 2 are light (1 each)
	for idx, n := range rows {
		for i := 0; i < n; i++ {
			if err := s.Insert(ctx, idx, s.NextID(), env(byte(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	next := func(sc *Scheduler, n int) []int {
		var out []int
		for i := 0; i < n; i++ {
			idx, ok := sc.Next()
			if !ok {
				out = append(out, -1)
				continue
			}
			out = append(out, idx)
		}
		return out
	}
	eq := func(name string, got, want []int) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", name, got, want)
				return
			}
		}
	}
	// share 2: two light turns (tenants 1, 2 from cursor A), then one heavy turn (tenant 0 from cursor B), repeated
	sc := NewScheduler(SchedulerConfig{Store: s, Gate: alwaysHot{}, Share: fixedShare{2}, Tenants: []string{"t-0000", "t-0001", "t-0002"}, TwoClass: true, LightTurns: 2})
	eq("interleave", next(sc, 6), []int{1, 2, 0, 1, 2, 0})
	// work-conserving: share 0 makes every tenant heavy, so light turns fall through to cursor B in ring order
	sc = NewScheduler(SchedulerConfig{Store: s, Gate: alwaysHot{}, Share: fixedShare{0}, Tenants: []string{"t-0000", "t-0001", "t-0002"}, TwoClass: true, LightTurns: 2})
	eq("fall-through", next(sc, 4), []int{0, 1, 2, 0})
	// a double miss returns false and leaves the cursors and the turn untouched
	sc = NewScheduler(SchedulerConfig{Store: s, Gate: neverHot{}, Share: fixedShare{2}, Tenants: []string{"t-0000", "t-0001", "t-0002"}, TwoClass: true, LightTurns: 2})
	eq("double miss", next(sc, 2), []int{-1, -1})
	if sc.cursorA != 0 || sc.cursorB != 0 || sc.turn != 0 {
		t.Errorf("state moved on a double miss: A=%d B=%d turn=%d", sc.cursorA, sc.cursorB, sc.turn)
	}
	// the pass off (TwoClass false, or LightTurns < 1, or no Share): the M1 ring, one turn per tenant per pass
	for name, cfg := range map[string]SchedulerConfig{
		"two-class off":   {Store: s, Gate: alwaysHot{}, Share: fixedShare{2}, Tenants: []string{"t-0000", "t-0001", "t-0002"}},
		"light turns 0":   {Store: s, Gate: alwaysHot{}, Share: fixedShare{2}, Tenants: []string{"t-0000", "t-0001", "t-0002"}, TwoClass: true},
		"no share source": {Store: s, Gate: alwaysHot{}, Tenants: []string{"t-0000", "t-0001", "t-0002"}, TwoClass: true, LightTurns: 2},
	} {
		eq(name, next(NewScheduler(cfg), 4), []int{0, 1, 2, 0})
	}
}
