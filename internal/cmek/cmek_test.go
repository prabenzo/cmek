// Owner: Claude (reviewed by Ben)
package cmek

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prabenzo/cmek/internal/kms"
)

func testHandle(t *testing.T, dekID string, validUntil time.Time) Handle {
	t.Helper()
	h := Handle{DEKID: dekID, ValidUntil: validUntil}
	for i := range h.key {
		h.key[i] = byte(i * 7)
	}
	return h
}

// TestEnvelope: round trip; wrong tenant, wrong msgID, wrong dekID in the AAD -> ErrPoison; a stale handle -> ErrLeaseExpired from Seal and Open.
func TestEnvelope(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	h := testHandle(t, "t-0042/1", now.Add(29*time.Second))
	pt := []byte(`{"event":"x","canary":"PLAINTEXT-CANARY-t-0042"}`)
	env, err := Seal(h, now, "t-0042", 123, pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(env.Ciphertext, []byte("PLAINTEXT-CANARY")) {
		t.Fatal("ciphertext contains the canary")
	}
	rows := []struct {
		name   string
		tenant string
		msgID  int64
		env    Envelope
		want   error
	}{
		{"round trip", "t-0042", 123, env, nil},
		{"wrong tenant", "t-0043", 123, env, ErrPoison},
		{"wrong msgID", "t-0042", 124, env, ErrPoison},
		{"wrong dekID", "t-0042", 123, Envelope{DEKID: "t-0042/2", Nonce: env.Nonce, Ciphertext: env.Ciphertext}, ErrPoison},
	}
	for _, r := range rows {
		hh := h
		if r.env.DEKID != h.DEKID {
			hh.DEKID = r.env.DEKID // same key, different id: only the AAD differs
		}
		got, err := Open(hh, now, r.tenant, r.msgID, r.env)
		if !errors.Is(err, r.want) {
			t.Errorf("%s: err = %v, want %v", r.name, err, r.want)
		}
		if r.want == nil && !bytes.Equal(got, pt) {
			t.Errorf("%s: plaintext mismatch", r.name)
		}
	}
	// a stale handle is refused on both sides
	stale := now.Add(29 * time.Second)
	if _, err := Seal(h, stale, "t-0042", 1, pt); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("stale seal: err = %v, want ErrLeaseExpired", err)
	}
	if _, err := Open(h, stale, "t-0042", 123, env); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("stale open: err = %v, want ErrLeaseExpired", err)
	}
	// a flipped ciphertext byte is poison; two seals of the same plaintext differ (fresh nonce)
	bad := env
	bad.Ciphertext = append([]byte(nil), env.Ciphertext...)
	bad.Ciphertext[0] ^= 1
	if _, err := Open(h, now, "t-0042", 123, bad); !errors.Is(err, ErrPoison) {
		t.Errorf("flipped byte: err = %v, want ErrPoison", err)
	}
	env2, _ := Seal(h, now, "t-0042", 123, pt)
	if env2.Nonce == env.Nonce {
		t.Error("two seals reused a nonce")
	}
}

// ---- M2 rig: a settable clock, a call-counting KMS decorator, a recording auditor, a map-backed DEK store ----

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After fires at once: the rig has no latency; latentKMS advances the clock instead.
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.Now().Add(d)
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// latentKMS counts calls and moves the clock by d before delegating, so a reply lands d after its send.
type latentKMS struct {
	kms.KMS
	clk   *fakeClock
	d     time.Duration
	calls atomic.Int64
}

func (l *latentKMS) GenerateDataKey(ctx context.Context, kekID string) (kms.DataKey, error) {
	l.calls.Add(1)
	l.clk.Advance(l.d)
	return l.KMS.GenerateDataKey(ctx, kekID)
}

func (l *latentKMS) Unwrap(ctx context.Context, kekID string, v int, wrapped []byte) ([32]byte, error) {
	l.calls.Add(1)
	l.clk.Advance(l.d)
	return l.KMS.Unwrap(ctx, kekID, v, wrapped)
}

type recorder struct {
	mu      sync.Mutex
	entries []Audit
}

func (r *recorder) Audit(e Audit) {
	r.mu.Lock()
	r.entries = append(r.entries, e)
	r.mu.Unlock()
}

func (r *recorder) count(op, outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.entries {
		if e.Op == op && (outcome == "" || e.Outcome == outcome) {
			n++
		}
	}
	return n
}

func (r *recorder) states(to State) []Audit {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Audit
	for _, e := range r.entries {
		if e.Op == "state" && e.To == to {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

type mapStore struct {
	mu          sync.Mutex
	rec         *recorder
	puts        []WrappedDEK
	auditsAtPut int
}

func (s *mapStore) PutDEK(ctx context.Context, d WrappedDEK) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, d)
	s.auditsAtPut = s.rec.len()
	return nil
}

type fixed struct{ u float64 }

func (f fixed) Float64() float64 { return f.u }

const (
	rigTenant = "t-0000"
	rigKEK    = "kek-t-0000"
)

type rig struct {
	m     *Manager
	fake  *kms.Fake
	lat   *latentKMS
	rec   *recorder
	store *mapStore
	clk   *fakeClock
	t0    time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	fake := kms.NewFake(kms.FakeConfig{Providers: []string{"gcp"}, KEKs: []kms.KEKSpec{{ID: rigKEK, Provider: "gcp", Idx: 0}}, Clock: clk, Rand: rand.New(rand.NewSource(1)), Lock: &sync.Mutex{}})
	lat := &latentKMS{KMS: fake, clk: clk}
	rec := &recorder{}
	store := &mapStore{rec: rec}
	m := New(context.Background(), Config{
		Tenants: []TenantSpec{{ID: rigTenant, Provider: "gcp", KEKID: rigKEK}}, Providers: []string{"gcp"},
		Keys: lat, Store: store, Audit: rec, Clock: clk, Jitter: fixed{0.5}, Spawn: func(f func()) { f() },
		Lease: 30 * time.Second, SoftTTL: 15 * time.Second, EarlyExpiry: time.Second, KMSTimeout: 500 * time.Millisecond,
		BackoffMin: 500 * time.Millisecond, BackoffMax: 8 * time.Second, BackoffJitter: 0.5, RevokedReprobe: 5 * time.Second,
		DEKMaxMessages: 10000, DEKMaxAge: 10 * time.Minute, SweepInterval: 250 * time.Millisecond,
		ProviderInflight: 32, TenantInflight: 2, IngestWaiters: 4,
	})
	return &rig{m: m, fake: fake, lat: lat, rec: rec, store: store, clk: clk, t0: clk.Now()}
}

func (r *rig) calls() int64 { return r.lat.calls.Load() }

func (r *rig) state() State { return r.m.Info(rigTenant).State }

// TestClassify: the down-versus-revoked table, one row per input.
func TestClassify(t *testing.T) {
	rows := []struct {
		name string
		err  error
		want Class
	}{
		{"nil", nil, OK},
		{"poison", ErrPoison, Poison},
		{"deadline", context.DeadlineExceeded, Transient},
		{"canceled", context.Canceled, Transient},
		{"timeout", &kms.Error{Code: kms.Timeout}, Transient},
		{"unavailable", &kms.Error{Code: kms.Unavailable}, Transient},
		{"access denied", &kms.Error{Code: kms.AccessDenied}, Deny},
		{"key disabled", &kms.Error{Code: kms.KeyDisabled}, Deny},
		{"unknown", errors.New("x"), Transient},
		{"wrapped deny", fmt.Errorf("call: %w", &kms.Error{Code: kms.KeyDisabled}), Deny},
		{"wrapped poison", fmt.Errorf("open: %w", ErrPoison), Poison},
	}
	for _, r := range rows {
		if got := Classify(r.err); got != r.want {
			t.Errorf("%s: Classify = %v, want %v", r.name, got, r.want)
		}
	}
}

// TestLease: the lease window runs from the send instant; Backoff doubles from 0.5 s to the 8 s cap with ±50 % jitter.
func TestLease(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	var zero Lease
	if zero.Usable(t0) {
		t.Error("zero lease is usable")
	}
	l := Lease{TTL: 30 * time.Second, SoftTTL: 15 * time.Second, Early: time.Second}
	if !l.Renew(t0) {
		t.Fatal("first Renew returned false")
	}
	checks := []struct {
		name string
		got  bool
		want bool
	}{
		{"usable at 28.999 s", l.Usable(t0.Add(28999 * time.Millisecond)), true},
		{"usable at 29 s", l.Usable(t0.Add(29 * time.Second)), false},
		{"soft due at 14.999 s", l.SoftDue(t0.Add(14999 * time.Millisecond)), false},
		{"soft due at 15 s", l.SoftDue(t0.Add(15 * time.Second)), true},
		{"remaining 29 s at t0", l.Remaining(t0) == 29*time.Second, true},
		{"remaining ≤ 0 at 29 s", l.Remaining(t0.Add(29*time.Second)) <= 0, true},
		{"renew backwards", l.Renew(t0.Add(-time.Second)), false},
		{"sentAt kept", l.SentAt.Equal(t0), true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := Backoff(i+1, 500*time.Millisecond, 8*time.Second, 0.5, 0.5); got != w {
			t.Errorf("Backoff(%d, u=0.5) = %v, want %v", i+1, got, w)
		}
		if got := Backoff(i+1, 500*time.Millisecond, 8*time.Second, 0.5, 0); got != w/2 {
			t.Errorf("Backoff(%d, u=0) = %v, want %v", i+1, got, w/2)
		}
		if got := Backoff(i+1, 500*time.Millisecond, 8*time.Second, 0.5, 1); got != w*3/2 {
			t.Errorf("Backoff(%d, u→1) = %v, want %v", i+1, got, w*3/2)
		}
	}
}

// TestManagerWalk drives one tenant through every state transition with inline Spawn and a settable clock:
// cold generate, lazy renewal from the send instant, the scheduler's kick, ride-through with the backoff schedule,
// fail-closed at the hard TTL, recovery, a foreign dek_id, revocation detected by the next lazy renewal, the fixed
// 5 s re-probe with one timeline episode, and restore. Rows follow docs/plan/M2.md › Tests.
func TestManagerWalk(t *testing.T) {
	r := newRig(t)
	m, clk, t0 := r.m, r.clk, r.t0
	ctx := context.Background()
	mustEncrypt := func(row string) Handle {
		t.Helper()
		h, err := m.EncryptKey(ctx, rigTenant)
		if err != nil {
			t.Fatalf("%s: EncryptKey: %v", row, err)
		}
		return h
	}
	expect := func(row string, cond bool, msg string, args ...any) {
		t.Helper()
		if !cond {
			t.Errorf("%s: "+msg, append([]any{row}, args...)...)
		}
	}

	// (1) cold path: one GenerateDataKey, ACTIVE, PutDEK once with the handle's id, then the audit
	h := mustEncrypt("row 1")
	expect("row 1", r.calls() == 1, "calls = %d, want 1", r.calls())
	expect("row 1", r.state() == Active, "state = %v", r.state())
	expect("row 1", len(r.store.puts) == 1 && r.store.puts[0].ID == h.DEKID, "PutDEK puts = %v, handle %q", r.store.puts, h.DEKID)
	expect("row 1", r.store.auditsAtPut == 0 && r.rec.count("generate", "ok") == 1, "generate audits: at put %d, after %d", r.store.auditsAtPut, r.rec.count("generate", "ok"))
	expect("row 1", h.ValidUntil.Equal(t0.Add(29*time.Second)), "ValidUntil = %v", h.ValidUntil)
	expect("row 1", m.Hot(rigTenant), "Hot = false")
	dekID := h.DEKID

	// (2) 16 s later the lease is soft-due: the handle is copied first, then one inline Unwrap renews from the send instant
	clk.Advance(16 * time.Second)
	r.lat.d = 2 * time.Second
	h = mustEncrypt("row 2")
	expect("row 2", r.calls() == 2, "calls = %d, want 2", r.calls())
	expect("row 2", h.ValidUntil.Equal(t0.Add(29*time.Second)), "ValidUntil = %v, want t0+29s", h.ValidUntil)
	expect("row 2", m.Info(rigTenant).LeaseRemaining == 27*time.Second, "LeaseRemaining = %v, want 27s", m.Info(rigTenant).LeaseRemaining)
	h = mustEncrypt("row 2")
	expect("row 2", r.calls() == 2, "third EncryptKey made a call")
	expect("row 2", h.ValidUntil.Equal(t0.Add(45*time.Second)), "ValidUntil = %v, want t0+45s", h.ValidUntil)

	// (3) the scheduler's visit kicks the lazy renewal for an ACTIVE tenant with backlog
	r.lat.d = 0
	clk.Advance(16 * time.Second) // t0+34
	expect("row 3", m.Hot(rigTenant), "Hot = false")
	expect("row 3", r.calls() == 3, "calls = %d, want 3", r.calls())
	expect("row 3", m.Info(rigTenant).Attempt == 0, "attempt = %d", m.Info(rigTenant).Attempt)

	// (4) gcp fast-fails: the next lazy renewal is transient; the tenant rides through and still seals
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{Mode: kms.ModeFastFail})
	clk.Advance(16 * time.Second) // t0+50 = T
	mustEncrypt("row 4")
	T := clk.Now()
	expect("row 4", r.state() == RidingThrough, "state = %v", r.state())
	expect("row 4", m.Info(rigTenant).Attempt == 1, "attempt = %d", m.Info(rigTenant).Attempt)
	expect("row 4", m.Hot(rigTenant), "Hot = false")
	expect("row 4", r.calls() == 4, "calls = %d, want 4", r.calls())

	// (5) Tick every 250 ms: probes only at T+0.5, 1.5, 3.5, 7.5 s (0.5, 1, 2, 4 s apart with fixed jitter)
	probeAt := map[time.Duration]bool{500 * time.Millisecond: true, 1500 * time.Millisecond: true, 3500 * time.Millisecond: true, 7500 * time.Millisecond: true}
	want := r.calls()
	for i := 1; i <= 52; i++ { // through T+13 s
		clk.Advance(250 * time.Millisecond)
		m.Tick(clk.Now())
		off := clk.Now().Sub(T)
		if probeAt[off] {
			want++
		}
		expect("row 5", r.calls() == want, "at T+%v calls = %d, want %d", off, r.calls(), want)
		if off < 13*time.Second {
			expect("row 5", r.state() == RidingThrough, "at T+%v state = %v", off, r.state())
		}
	}
	// (6) the Tick at last-OK sentAt + 29 s (T+13) fails closed: purge, KEY_UNAVAILABLE, no calls
	expect("row 6", r.state() == KeyUnavailable, "state = %v", r.state())
	if s := r.rec.states(KeyUnavailable); len(s) != 1 || s[0].Purged != 1 {
		t.Errorf("row 6: KEY_UNAVAILABLE state audits = %+v, want one with Purged 1", s)
	}
	expect("row 6", !m.Hot(rigTenant), "Hot = true")
	if _, err := m.DecryptKey(rigTenant, dekID); !errors.Is(err, ErrLeaseExpired) {
		t.Errorf("row 6: DecryptKey err = %v, want ErrLeaseExpired", err)
	}
	if _, err := m.EncryptKey(ctx, rigTenant); !errors.Is(err, ErrKeyUnavailable) {
		t.Errorf("row 6: EncryptKey err = %v, want ErrKeyUnavailable", err)
	}
	expect("row 6", r.calls() == want, "calls = %d, want %d (no call while parked)", r.calls(), want)

	// (7) fault cleared: the Tick past nextProbeAt (T+15.5) heals the tenant
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{})
	for clk.Now().Sub(T) < 15500*time.Millisecond {
		clk.Advance(250 * time.Millisecond)
		m.Tick(clk.Now())
		if off := clk.Now().Sub(T); off < 15500*time.Millisecond {
			expect("row 7", r.calls() == want, "at T+%v calls = %d, want %d", off, r.calls(), want)
		}
	}
	want++
	expect("row 7", r.calls() == want, "calls = %d, want %d", r.calls(), want)
	expect("row 7", r.state() == Active, "state = %v", r.state())
	if _, err := m.DecryptKey(rigTenant, dekID); err != nil {
		t.Errorf("row 7: DecryptKey err = %v", err)
	}
	expect("row 7", m.Hot(rigTenant), "Hot = false")

	// (8) a dek_id the tenant does not own is poison
	if _, err := m.DecryptKey(rigTenant, "other/1"); !errors.Is(err, ErrPoison) {
		t.Errorf("row 8: DecryptKey err = %v, want ErrPoison", err)
	}

	// (9) revoke: the next lazy renewal is the detection (Tick never probes an ACTIVE tenant)
	r.fake.Revoke(rigKEK)
	clk.Advance(16 * time.Second)
	h = mustEncrypt("row 9")
	expect("row 9", h.ValidUntil.Equal(T.Add(15500*time.Millisecond+29*time.Second)), "ValidUntil = %v", h.ValidUntil)
	want++
	expect("row 9", r.calls() == want, "calls = %d, want %d", r.calls(), want)
	expect("row 9", r.state() == Revoked, "state = %v", r.state())
	if s := r.rec.states(Revoked); len(s) != 1 || s[0].Purged != 1 {
		t.Errorf("row 9: REVOKED state audits = %+v, want exactly one with Purged 1", s)
	}
	if _, err := m.EncryptKey(ctx, rigTenant); !errors.Is(err, ErrKeyRevoked) {
		t.Errorf("row 9: EncryptKey #2 err = %v, want ErrKeyRevoked", err)
	}
	if _, err := m.DecryptKey(rigTenant, dekID); !errors.Is(err, ErrKeyRevoked) {
		t.Errorf("row 9: DecryptKey err = %v, want ErrKeyRevoked", err)
	}
	expect("row 9", !m.Hot(rigTenant), "Hot = true")

	// (10) three re-probes 5 s apart: three Deny audits, still one REVOKED line, nothing in between
	denies := r.rec.count("unwrap", "denied")
	for i := 0; i < 3; i++ {
		clk.Advance(2500 * time.Millisecond)
		m.Tick(clk.Now())
		expect("row 10", r.calls() == want, "call between re-probes")
		clk.Advance(2500 * time.Millisecond)
		m.Tick(clk.Now())
		want++
		expect("row 10", r.calls() == want, "re-probe %d: calls = %d, want %d", i+1, r.calls(), want)
	}
	expect("row 10", r.rec.count("unwrap", "denied") == denies+3, "deny audits = %d, want %d", r.rec.count("unwrap", "denied"), denies+3)
	expect("row 10", len(r.rec.states(Revoked)) == 1, "REVOKED lines = %d", len(r.rec.states(Revoked)))
	expect("row 10", r.state() == Revoked, "state = %v", r.state())

	// (11) restore: the next 5 s re-probe brings the tenant back
	r.fake.Restore(rigKEK)
	clk.Advance(5 * time.Second)
	m.Tick(clk.Now())
	expect("row 11", r.state() == Active, "state = %v", r.state())
	if _, err := m.DecryptKey(rigTenant, dekID); err != nil {
		t.Errorf("row 11: DecryptKey err = %v", err)
	}
	expect("row 11", m.Hot(rigTenant), "Hot = false")
}
