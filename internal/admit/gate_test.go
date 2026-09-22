// Owner: Claude
package admit

import (
	"errors"
	"testing"
	"time"
)

type fakeBacklog struct {
	per        []int
	total      int
	backlogged int
}

func (b *fakeBacklog) Backlog(idx int) int { return b.per[idx] }
func (b *fakeBacklog) Total() int          { return b.total }
func (b *fakeBacklog) Backlogged() int     { return b.backlogged }

type frozen struct{ t time.Time }

func (f frozen) Now() time.Time { return f.t }

// TestGate: the check order (global cap → bucket → tenant cap), the share class from the one backlog read, overload
// dealt only to over-share tenants, and the FairShare = false cut (everyone shed, classed within).
func TestGate(t *testing.T) {
	clk := frozen{time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	newGate := func(fair bool) (*Gate, *fakeBacklog) {
		b := &fakeBacklog{per: make([]int, 4)}
		return New(Config{Tenants: 4, Rate: 100, Burst: 2, TenantCap: 50, GlobalCap: 1000, FairShare: fair, Backlog: b, Clock: clk}), b
	}
	g, b := newGate(true)
	// idle: within share, admitted; the frozen clock never refills, so the burst of 2 is all a tenant gets
	if within, err := g.Admit(0); err != nil || !within {
		t.Fatalf("idle: within=%v err=%v", within, err)
	}
	if fs := g.FairShare(); fs != 1000 {
		t.Errorf("FairShare with nothing backlogged = %d, want 1000", fs)
	}
	// share: 1000 / 4 backlogged = 250; tenant 1 holds 300 (over), tenant 2 holds 10 (within)
	b.backlogged, b.per[1], b.per[2] = 4, 300, 10
	if fs := g.FairShare(); fs != 250 {
		t.Errorf("FairShare = %d, want 250", fs)
	}
	if !g.WithinShare(2) || g.WithinShare(1) {
		t.Errorf("WithinShare: tenant 2 %v, tenant 1 %v", g.WithinShare(2), g.WithinShare(1))
	}
	// below the global cap the over-share tenant is judged by its own caps: backlog 300 ≥ 50 → backlog_full, over share
	if within, err := g.Admit(1); !errors.Is(err, ErrBacklogFull) || within {
		t.Errorf("over-share tenant under the cap: within=%v err=%v, want backlog_full/over", within, err)
	}
	// at the global cap only the over-share tenant is shed; the within-share one still passes the cap (then its bucket)
	b.total = 1000
	if within, err := g.Admit(1); !errors.Is(err, ErrOverloaded) || within {
		t.Errorf("over-share at cap: within=%v err=%v, want overloaded/over", within, err)
	}
	if within, err := g.Admit(2); err != nil || !within {
		t.Errorf("within-share at cap: within=%v err=%v, want admitted", within, err)
	}
	// the bucket: burst 2, second token used above; the third call is rate limited, classed within
	if within, err := g.Admit(2); err != nil || !within {
		t.Errorf("second token: within=%v err=%v", within, err)
	}
	if within, err := g.Admit(2); !errors.Is(err, ErrRateLimited) || !within {
		t.Errorf("bucket empty: within=%v err=%v, want rate_limited/within", within, err)
	}
	// the global cap is checked before the bucket: an over-share tenant with an empty bucket reads overloaded
	b.per[3] = 300
	g.Admit(3)
	g.Admit(3)
	if _, err := g.Admit(3); !errors.Is(err, ErrOverloaded) {
		t.Errorf("cap before bucket: err=%v, want overloaded", err)
	}
	// the cut: FairShare = false sheds everyone at the cap and classes them within (MaxInt share)
	g, b = newGate(false)
	b.total, b.backlogged, b.per[0] = 1000, 4, 1
	if within, err := g.Admit(0); !errors.Is(err, ErrOverloaded) || !within {
		t.Errorf("cut at cap: within=%v err=%v, want overloaded/within", within, err)
	}
	b.total = 999
	if within, err := g.Admit(0); err != nil || !within {
		t.Errorf("cut under cap: within=%v err=%v", within, err)
	}
}
