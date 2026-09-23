// Owner: Claude (reviewed by Ben)
package check

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prabenzo/cmek/internal/kms"
	"github.com/prabenzo/cmek/internal/queue"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time                         { return c.now }
func (c *fakeClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type fakeRows struct {
	hits   int
	census queue.Census
}

func (r *fakeRows) CanaryFull(context.Context, []byte) (int, error) { return r.hits, nil }
func (r *fakeRows) Census(_ context.Context, under func()) (queue.Census, error) {
	if under != nil {
		under()
	}
	return r.census, nil
}

type fakeDeliveries struct {
	total      []int64
	sink       []int64
	between    int64
	overrun    bool
	mismatches int64
	calls      []time.Time // the (lo, hi) pairs DeliveredBetween was asked for
}

func (d *fakeDeliveries) Delivered(idx int) int64  { return d.total[idx] }
func (d *fakeDeliveries) DeliveredAll(dst []int64) { copy(dst, d.sink) }
func (d *fakeDeliveries) DeliveredBetween(idx int, sinceSeq int64, lo, hi time.Time) (int64, int64, bool) {
	d.calls = append(d.calls, lo, hi)
	return d.between, d.total[idx], d.overrun
}
func (d *fakeDeliveries) Mismatches() int64 { return d.mismatches }

type fakeTruth struct{ events []kms.KeyEvent }

func (t *fakeTruth) KeyEvents() []kms.KeyEvent { return t.events }

type report struct {
	ok     bool
	n      int64
	at     time.Time
	detail string
}

type fakeReporter struct {
	last  map[string]report
	lines []string
}

func (r *fakeReporter) Report(id string, ok bool, n int64, at time.Time, detail string) {
	if r.last == nil {
		r.last = map[string]report{}
	}
	r.last[id] = report{ok, n, at, detail}
}
func (r *fakeReporter) Timeline(text string) { r.lines = append(r.lines, text) }

type fakeHealth struct {
	detectedAt time.Time
	detected   bool
	cur, base  time.Duration // HealthyP99 (ok when base > 0)
	rej        int64
	scenario   string // Scenario (running when non-empty)
	since      time.Time
}

func (h *fakeHealth) HealthyP99() (time.Duration, time.Duration, bool) {
	return h.cur, h.base, h.base > 0
}
func (h *fakeHealth) HealthyRejections() int64     { return h.rej }
func (h *fakeHealth) OverloadedWithinShare() int64 { return 0 }
func (h *fakeHealth) DeliveredPS() float64         { return 0 }
func (h *fakeHealth) Scenario() (string, bool, time.Time) {
	return h.scenario, h.scenario != "", h.since
}
func (h *fakeHealth) DetectedRevokedAt(int) (time.Time, int, bool) {
	return h.detectedAt, 1, h.detected
}

func cleanCensus(n int) queue.Census {
	c := queue.Census{Accepted: make([]int64, n), Delivered: make([]int64, n), Expired: make([]int64, n), Ready: make([]int64, n), Claimed: make([]int64, n), Dead: make([]int64, n)}
	for i := range c.Accepted {
		c.Accepted[i], c.Delivered[i], c.Ready[i], c.Claimed[i] = 10, 6, 3, 1
	}
	return c
}

// TestCheckerTurnsRed: the checker can go red. Each light is driven red from fakes and back to green on a clean
// pass, S3's sinceSeq initialisation keeps a long-lived tenant off "coverage gap", a restore closes the window, and
// the ground-truth detection line is posted once per episode.
func TestCheckerTurnsRed(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 12, 0, 59, 0, time.UTC)
	clk := &fakeClock{now: t0}
	tenants := []string{"t-0042", "t-0043"}
	rows := &fakeRows{census: cleanCensus(2)}
	del := &fakeDeliveries{total: []int64{5000, 10}, sink: []int64{6, 6}}
	truth := &fakeTruth{}
	rep := &fakeReporter{}
	health := &fakeHealth{}
	c := New(Config{Tenants: tenants, Rows: rows, Deliveries: del, Truth: truth, Health: health, Reporter: rep, Clock: clk,
		Lease: 30 * time.Second, Interval: time.Second, FullScanEvery: 5 * time.Second, CanaryPrefix: "PLAINTEXT-CANARY-"})
	ctx := context.Background()

	// (4) all clean: four greens stamped with the clock, L1 and L4 absent (flags off)
	c.Once(ctx)
	for _, id := range []string{"S1", "S2", "S3", "S4"} {
		r, ok := rep.last[id]
		if !ok || !r.ok || r.n != 0 || !r.at.Equal(t0) {
			t.Errorf("%s clean: %+v (present %v)", id, r, ok)
		}
	}
	if _, ok := rep.last["L1"]; ok {
		t.Error("L1 reported with its flag off")
	}

	// (1) S1: one canary row ⇒ red, n = 1, and it stays red on a later clean scan (cumulative)
	rows.hits = 1
	clk.now = t0.Add(5 * time.Second)
	c.Once(ctx)
	if r := rep.last["S1"]; r.ok || r.n != 1 || r.detail != "1 rows" {
		t.Errorf("S1 after a hit: %+v", r)
	}
	rows.hits = 0
	clk.now = t0.Add(10 * time.Second)
	c.Once(ctx)
	if r := rep.last["S1"]; r.ok || r.n != 1 {
		t.Errorf("S1 stays red: %+v", r)
	}

	// S2: a mismatch ⇒ red
	del.mismatches = 2
	c.Once(ctx)
	if r := rep.last["S2"]; r.ok || r.n != 2 {
		t.Errorf("S2: %+v", r)
	}

	// (2) S3: a revoke at t0 first seen with 5,000 deliveries behind it: the resume point is the tenant's total, so
	// an empty window is green and not a coverage gap; the window asked for is (t0 + 30 s, +∞)
	truth.events = []kms.KeyEvent{{KEKID: "kek-t-0042", Idx: 0, At: t0, Enabled: false}}
	del.between, del.overrun = 0, false
	health.detectedAt, health.detected = t0.Add(8200*time.Millisecond), true
	c.Once(ctx)
	if r := rep.last["S3"]; !r.ok || r.n != 0 || r.detail != "" {
		t.Errorf("S3 empty window: %+v", r)
	}
	if got := del.calls; len(got) < 2 || !got[len(got)-2].Equal(t0.Add(30*time.Second)) || !got[len(got)-1].IsZero() {
		t.Errorf("S3 window asked for: %v", del.calls)
	}
	if len(rep.lines) != 1 || !strings.HasPrefix(rep.lines[0], "t-0042 revoked at 12:00:59 (ground truth), detected in 8.2 s") {
		t.Errorf("detection line: %q", rep.lines)
	}
	c.Once(ctx)
	if len(rep.lines) != 1 {
		t.Errorf("detection line posted again: %q", rep.lines)
	}
	// one delivery inside the window ⇒ red; it stays counted after the sink goes quiet
	del.between = 1
	c.Once(ctx)
	if r := rep.last["S3"]; r.ok || r.n != 1 {
		t.Errorf("S3 late decrypt: %+v", r)
	}
	del.between = 0
	c.Once(ctx)
	if r := rep.last["S3"]; r.ok || r.n != 1 {
		t.Errorf("S3 stays red: %+v", r)
	}
	// a coverage gap is red even with nothing counted
	c2 := New(Config{Tenants: tenants, Rows: rows, Deliveries: del, Truth: truth, Reporter: rep, Clock: clk, Lease: 30 * time.Second, Interval: time.Second, FullScanEvery: 5 * time.Second})
	del.overrun = true
	c2.Once(ctx)
	if r := rep.last["S3"]; r.ok || r.detail != "coverage gap" {
		t.Errorf("S3 overrun: %+v", r)
	}
	del.overrun = false
	// a restore at t0 + 40 s closes the window: a delivery at t0 + 45 s is the backlog draining, so the checker
	// asks the sink for (t0 + 30 s, t0 + 40 s] and a fake that answers 0 there keeps a fresh checker green
	truth.events = append(truth.events, kms.KeyEvent{KEKID: "kek-t-0042", Idx: 0, At: t0.Add(40 * time.Second), Enabled: true})
	c3 := New(Config{Tenants: tenants, Rows: rows, Deliveries: del, Truth: truth, Reporter: rep, Clock: clk, Lease: 30 * time.Second, Interval: time.Second, FullScanEvery: 5 * time.Second})
	del.calls = nil
	c3.Once(ctx)
	if r := rep.last["S3"]; !r.ok {
		t.Errorf("S3 after restore: %+v", r)
	}
	if got := del.calls; len(got) != 2 || !got[0].Equal(t0.Add(30*time.Second)) || !got[1].Equal(t0.Add(40*time.Second)) {
		t.Errorf("S3 closed window asked for: %v", got)
	}

	// (3) S4: accepted 5 against 4 accounted for ⇒ red naming the tenant; a sink count above delivered + claimed too
	bad := cleanCensus(2)
	bad.Accepted[1] = 5
	bad.Delivered[1], bad.Ready[1], bad.Claimed[1] = 2, 1, 1
	rows.census = bad
	del.sink = []int64{6, 2}
	c.Once(ctx)
	if r := rep.last["S4"]; r.ok || r.n != 1 || r.detail != "t-0043 off by 1" {
		t.Errorf("S4 conservation: %+v", r)
	}
	rows.census = cleanCensus(2)
	del.sink = []int64{8, 6} // t-0042: delivered 6, claimed 1 ⇒ sink must be within [6, 7]
	c.Once(ctx)
	if r := rep.last["S4"]; r.ok || r.n != 2 || !strings.HasPrefix(r.detail, "t-0042 sink 8 outside [6, 7]") {
		t.Errorf("S4 sink bound: %+v", r)
	}
}

// TestWindows pins the ground-truth pairing: a revoke opens an episode, the next restore of the same KEK closes it,
// a repeated revoke inside an open episode is ignored, and a still-revoked key has a zero restore.
func TestWindows(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	ev := []kms.KeyEvent{
		{Idx: 1, At: t0, Enabled: false},
		{Idx: 0, At: t0.Add(time.Second), Enabled: false},
		{Idx: 1, At: t0.Add(2 * time.Second), Enabled: false},
		{Idx: 1, At: t0.Add(3 * time.Second), Enabled: true},
		{Idx: 1, At: t0.Add(4 * time.Second), Enabled: false},
		{Idx: 2, At: t0.Add(5 * time.Second), Enabled: true}, // a restore with no revoke: nothing
	}
	ws := windows(ev)
	want := []window{{0, t0.Add(time.Second), time.Time{}}, {1, t0, t0.Add(3 * time.Second)}, {1, t0.Add(4 * time.Second), time.Time{}}}
	if len(ws) != len(want) {
		t.Fatalf("windows = %+v", ws)
	}
	for i := range want {
		if ws[i] != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, ws[i], want[i])
		}
	}
}

// TestL1Persistence: L1 is green "idle" with no scenario and "grace" for L1Grace after one starts; a healthy p99 over
// its limit is a hover detail until it has lasted L1For, then red with n counting the red passes; a healthy rejection
// is red at once; back under the limit the persistence clock resets.
func TestL1Persistence(t *testing.T) {
	t0 := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: t0}
	rep := &fakeReporter{}
	health := &fakeHealth{cur: 20 * time.Millisecond, base: 20 * time.Millisecond}
	c := New(Config{Tenants: []string{"t-0042"}, Rows: &fakeRows{census: cleanCensus(1)}, Deliveries: &fakeDeliveries{total: []int64{10}, sink: []int64{6}},
		Truth: &fakeTruth{}, Health: health, Reporter: rep, Clock: clk, Lease: 30 * time.Second, Interval: time.Second, FullScanEvery: 5 * time.Second,
		CanaryPrefix: "PLAINTEXT-CANARY-", L1: true, L1Grace: 7 * time.Second, L1For: 3 * time.Second, L1Floor: 25 * time.Millisecond, L1Ratio: 1.25})
	ctx := context.Background()
	pass := func(sec int) report {
		clk.now = t0.Add(time.Duration(sec) * time.Second)
		c.Once(ctx)
		return rep.last["L1"]
	}
	if r := pass(0); !r.ok || r.detail != "idle" {
		t.Errorf("no scenario: %+v", r)
	}
	health.scenario, health.since = "tenant_surge", t0.Add(time.Second)
	health.cur = 67 * time.Millisecond // the opening burst, inside the grace
	if r := pass(5); !r.ok || r.detail != "grace" {
		t.Errorf("in grace: %+v", r)
	}
	// limit is 1.25 × max(20, 25) = 31.25 ms; 34 ms over it for two passes is a detail, the third pass is red
	health.cur = 34 * time.Millisecond
	if r := pass(8); !r.ok || r.n != 0 || r.detail != "p99 34ms > 31ms for 0 of 3 s" {
		t.Errorf("first pass over: %+v", r)
	}
	if r := pass(10); !r.ok || r.n != 0 {
		t.Errorf("second pass over: %+v", r)
	}
	if r := pass(11); r.ok || r.n != 1 || r.detail != "p99 34ms > 31ms for 3 s" {
		t.Errorf("third pass over: %+v", r)
	}
	if r := pass(12); r.ok || r.n != 2 {
		t.Errorf("stays red: %+v", r)
	}
	// back under the limit: green, and the next excursion starts its own clock
	health.cur = 30 * time.Millisecond
	if r := pass(13); !r.ok || r.n != 2 || r.detail != "" {
		t.Errorf("under again: %+v", r)
	}
	health.cur = 40 * time.Millisecond
	if r := pass(14); !r.ok || r.detail != "p99 40ms > 31ms for 0 of 3 s" {
		t.Errorf("new excursion: %+v", r)
	}
	// a healthy rejection is red at once, whatever the p99
	health.cur, health.rej = 20*time.Millisecond, 1
	if r := pass(15); r.ok || r.n != 3 || r.detail != "1 healthy rejections" {
		t.Errorf("rejection: %+v", r)
	}
	// the scenario ends: idle, the count kept
	health.scenario, health.rej = "", 0
	if r := pass(16); !r.ok || r.n != 3 || r.detail != "idle" {
		t.Errorf("idle after: %+v", r)
	}
	// a global surge offers load to every tenant: L1 is not judged, whatever the healthy p99 reads
	health.scenario, health.since, health.cur = "global_surge", t0.Add(17*time.Second), 4*time.Second
	if r := pass(30); !r.ok || r.n != 3 || r.detail != "global surge" {
		t.Errorf("global surge: %+v", r)
	}
}
