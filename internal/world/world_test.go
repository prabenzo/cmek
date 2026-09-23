// Owner: Claude (reviewed by Ben)
package world

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

// TestScenarioRunner: start, busy, unknown, stop (returns the name, restores the multiplier, clears the card and the
// targets), stop while idle, a natural end with its tail (recovery and drain stamped, the restored line), and the
// revocation's open phase ended by Restore.
func TestScenarioRunner(t *testing.T) {
	p := Small()
	p.DBDir = t.TempDir()
	p.TenantSurgeFor = 60 * time.Millisecond
	p.SnapshotInterval = 20 * time.Millisecond
	p.ScenarioTailMin, p.ScenarioTailMax = 100*time.Millisecond, 2*time.Second
	w, err := New(p, Deps{ID: "s"}) // the real clock: the tail measures elapsed time
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	defer w.Stop()
	surge := w.byRank[p.SurgeTenantRank-1]
	base := w.gen.Offered(surge)
	if err := w.StartScenario("nope"); !errors.Is(err, ErrUnknownScenario) {
		t.Errorf("unknown: %v", err)
	}
	if err := w.StartScenario("tenant_surge"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := w.StartScenario("global_surge"); !errors.Is(err, ErrBusy) {
		t.Errorf("second start: %v, want ErrBusy", err)
	}
	if name, running, _ := w.metrics.Scenario(); !running || name != "tenant_surge" {
		t.Errorf("card: %q running=%v", name, running)
	}
	if !w.metrics.Affected(surge) {
		t.Error("surge tenant not affected after start")
	}
	if got := w.gen.Offered(surge); math.Abs(got-base*p.TenantSurgeMult) > 1e-9 {
		t.Errorf("surge multiplier: offered %v, want %v", got, base*p.TenantSurgeMult)
	}
	if name := w.StopScenario(); name != "tenant_surge" {
		t.Errorf("stop returned %q", name)
	}
	if name, running, _ := w.metrics.Scenario(); running || name != "" {
		t.Errorf("card after stop: %q running=%v", name, running)
	}
	if got := w.gen.Offered(surge); got != base {
		t.Errorf("after stop: offered %v, want %v", got, base)
	}
	if w.metrics.Affected(surge) {
		t.Error("surge tenant still affected after stop")
	}
	if name := w.StopScenario(); name != "" {
		t.Errorf("idle stop returned %q", name)
	}
	// a natural end (60 ms phase) frees the slot and restores the multiplier by itself
	if err := w.StartScenario("tenant_surge"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	seenDone := false // the tail holds the card for ≥ ScenarioTailMin once recovery and drain are stamped
	for time.Now().Before(deadline) {
		if _, running, _ := w.metrics.Scenario(); !running {
			break
		}
		if _, _, done := w.metrics.Recovered(); done {
			seenDone = true
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, running, _ := w.metrics.Scenario(); running {
		t.Fatal("scenario still running 3 s after a 60 ms phase")
	}
	if !seenDone {
		t.Error("tail: Recovered never reported done while the card was up")
	}
	if got := w.gen.Offered(surge); got != base {
		t.Errorf("after natural end: offered %v, want %v", got, base)
	}
	if got := w.gen.Offered(surge); got != base {
		t.Errorf("after natural end: offered %v, want %v", got, base)
	}
	if err := w.StartScenario("global_surge"); err != nil { // the slot is free again
		t.Errorf("start after natural end: %v", err)
	}
	if name := w.StopScenario(); name != "global_surge" {
		t.Errorf("stop returned %q", name)
	}
	// key_revocation: an open phase that Restore (on the target only) ends, then the tail
	rev := w.byRank[p.RevokeTenantRank-1]
	if err := w.StartScenario("key_revocation"); err != nil {
		t.Fatalf("revocation: %v", err)
	}
	other := w.byRank[0]
	if err := w.SetKey(w.ids[other], "restore"); err != nil { // not the target: the run stays open
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if name, running, _ := w.metrics.Scenario(); !running || name != "key_revocation" {
		t.Errorf("revocation ended by a restore of another tenant: %q running=%v", name, running)
	}
	if err := w.SetKey(w.ids[rev], "restore"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, running, _ := w.metrics.Scenario(); !running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, running, _ := w.metrics.Scenario(); running {
		t.Fatal("revocation still running 3 s after Restore")
	}
}

// TestNoPlaintextAtRest (S1's mechanism): with no workers running, every ingested row sits in the messages table as
// a Tink ciphertext longer than the payload by the IV and the tag, and neither the canary nor any JSON of the payload
// appears in it; the deks table holds only wrapped keysets.
func TestNoPlaintextAtRest(t *testing.T) {
	dir := t.TempDir()
	p := Small()
	p.DBDir = dir
	w, err := New(p, Deps{ID: "rest", Clock: &testClock{now: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	const n = 12
	for i := 0; i < n; i++ {
		tenant := fmt.Sprintf("t-%04d", i%p.Tenants)
		payload := fmt.Sprintf(`{"canary":"%s%s","i":%d}`, p.CanaryPrefix, tenant, i)
		if err := w.Ingest(context.Background(), tenant, []byte(payload)); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, fmt.Sprintf("killswitch-%d-rest.db", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT tenant_id, ciphertext FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var tenant string
		var ct []byte
		if err := rows.Scan(&tenant, &ct); err != nil {
			t.Fatal(err)
		}
		count++
		for _, needle := range []string{p.CanaryPrefix, `"canary"`, `"i":`, tenant} {
			if bytes.Contains(ct, []byte(needle)) {
				t.Errorf("row of %s: ciphertext contains %q", tenant, needle)
			}
		}
		if min := 12 + len(`{"canary":"`) + len(p.CanaryPrefix) + 16; len(ct) < min {
			t.Errorf("row of %s: ciphertext is %d bytes, shorter than IV + payload + tag (%d)", tenant, len(ct), min)
		}
	}
	if count != n {
		t.Errorf("messages at rest = %d, want %d", count, n)
	}
	var deks int
	if err := db.QueryRow(`SELECT count(*) FROM deks WHERE length(wrapped_dek) > 64`).Scan(&deks); err != nil || deks < 1 {
		t.Errorf("wrapped deks = %d (%v), want at least one keyset-sized blob", deks, err)
	}
}

// TestNoCacheScenarios: the one-band run switches the cache off for its band only and the global run for everyone;
// both restore the cache on exit (a later ingest fills it again), the surge phase scales the generator and the
// exit resets it, and each run posts its summary line.
func TestNoCacheScenarios(t *testing.T) {
	p := Small()
	p.DBDir = t.TempDir()
	p.SnapshotInterval = 20 * time.Millisecond
	p.ScenarioTailMin, p.ScenarioTailMax = 100*time.Millisecond, 2*time.Second
	p.NoCacheFor, p.NoCacheBlip, p.NoCacheAfter = 60*time.Millisecond, 40*time.Millisecond, 60*time.Millisecond
	p.NoCacheSurgeLead, p.NoCacheSurgeFor, p.NoCacheSurgeAfter = 60*time.Millisecond, 80*time.Millisecond, 60*time.Millisecond
	w, err := New(p, Deps{ID: "nc"})
	if err != nil {
		t.Fatal(err)
	}
	w.Start()
	defer w.Stop()
	ctx := context.Background()
	band := w.band(p.NoCacheProvider)
	outside := 0
	for w.P.Providers[outside*len(w.P.Providers)/w.P.Tenants] == p.NoCacheProvider {
		outside++
	}
	in, out := w.ids[band[0]], w.ids[outside]
	payload := func(id string) []byte { return []byte(fmt.Sprintf(`{"canary":"%s%s"}`, p.CanaryPrefix, id)) }
	for _, id := range []string{in, out} {
		if err := w.Ingest(ctx, id, payload(id)); err != nil {
			t.Fatal(err)
		}
		if w.keys.Info(id).HotDEKs != 1 {
			t.Fatalf("%s: hot DEKs %d before the run", id, w.keys.Info(id).HotDEKs)
		}
	}
	waitIdle := func(what string) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, running, _ := w.metrics.Scenario(); !running {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s still running after 3 s", what)
	}
	lines := func() []string {
		ch, cancel := w.Subscribe()
		defer cancel()
		var s struct {
			Events []struct{ Text string } `json:"events"`
		}
		select {
		case b := <-ch:
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no snapshot")
		}
		out := make([]string, 0, len(s.Events))
		for _, e := range s.Events {
			out = append(out, e.Text)
		}
		return out
	}
	// one band + blip
	if err := w.StartScenario("no_cache"); err != nil {
		t.Fatal(err)
	}
	if got := w.keys.Info(in).HotDEKs; got != 0 {
		t.Errorf("band tenant keeps %d hot DEKs with the cache off", got)
	}
	if got := w.keys.Info(out).HotDEKs; got != 1 {
		t.Errorf("tenant outside the band lost its cache: hot %d", got)
	}
	if !w.metrics.Affected(band[0]) || w.metrics.Affected(outside) {
		t.Error("affected set is not the band")
	}
	if err := w.Ingest(ctx, in, payload(in)); err != nil { // a per-request call, accepted
		t.Errorf("ingest without the cache: %v", err)
	}
	waitIdle("no_cache")
	if err := w.Ingest(ctx, in, payload(in)); err != nil {
		t.Fatal(err)
	}
	if got := w.keys.Info(in).HotDEKs; got != 1 {
		t.Errorf("cache not back after the run: hot %d", got)
	}
	found := false
	for _, l := range lines() {
		found = found || strings.HasPrefix(l, "no cache ("+p.NoCacheProvider)
	}
	if !found {
		t.Error("the one-band summary line was not posted")
	}
	// everyone + surge
	rank1 := w.byRank[0]
	base := w.gen.Offered(rank1)
	if err := w.StartScenario("no_cache_surge"); err != nil {
		t.Fatal(err)
	}
	if w.keys.Info(in).HotDEKs != 0 || w.keys.Info(out).HotDEKs != 0 {
		t.Errorf("global run left a cache: in %d, out %d", w.keys.Info(in).HotDEKs, w.keys.Info(out).HotDEKs)
	}
	if !w.metrics.Affected(rank1) {
		t.Error("the heaviest tenant is not in the declared affected set")
	}
	surged := false
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !surged {
		surged = math.Abs(w.gen.Offered(rank1)-base*p.GlobalSurgeMult) < 1e-9
		time.Sleep(2 * time.Millisecond)
	}
	if !surged {
		t.Error("the surge phase never scaled the generator")
	}
	waitIdle("no_cache_surge")
	if got := w.gen.Offered(rank1); got != base {
		t.Errorf("after the run: offered %v, want %v", got, base)
	}
	if err := w.Ingest(ctx, out, payload(out)); err != nil {
		t.Fatal(err)
	}
	if got := w.keys.Info(out).HotDEKs; got != 1 {
		t.Errorf("cache not back after the global run: hot %d", got)
	}
	found = false
	for _, l := range lines() {
		found = found || strings.HasPrefix(l, "no cache (everyone, ")
	}
	if !found {
		t.Error("the global summary line was not posted")
	}
}
