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

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"

	"github.com/prabenzo/cmek/internal/kms"
)

// testKeyset is one fresh DEK keyset as the fetcher builds it, and its primitive.
func testKeyset(t *testing.T) (*keyset.Handle, tink.AEAD) {
	t.Helper()
	kh, err := keyset.NewHandle(aead.AES256GCMNoPrefixKeyTemplate())
	if err != nil {
		t.Fatal(err)
	}
	prim, err := aead.New(kh)
	if err != nil {
		t.Fatal(err)
	}
	return kh, prim
}

func testHandle(t *testing.T, dekID string, validUntil time.Time) (Handle, *keyset.Handle) {
	t.Helper()
	kh, prim := testKeyset(t)
	return Handle{DEKID: dekID, ValidUntil: validUntil, prim: prim}, kh
}

// TestEnvelope: round trip; wrong tenant, wrong msgID, wrong dekID in the AAD -> ErrPoison; a stale handle -> ErrLeaseExpired
// from Seal and Open; a flipped byte -> ErrPoison; a zeroed handle refuses; and the ciphertext is plain Tink AES-GCM: a
// primitive built independently from the same keyset opens it with our AAD.
func TestEnvelope(t *testing.T) {
	now := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	h, kh := testHandle(t, "t-0042/1", now.Add(29*time.Second))
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
		{"wrong dekID", "t-0042", 123, Envelope{DEKID: "t-0042/2", Ciphertext: env.Ciphertext}, ErrPoison},
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
	if bytes.Equal(env2.Ciphertext, env.Ciphertext) {
		t.Error("two seals of one plaintext produced one ciphertext (IV reuse)")
	}
	// a zeroed handle refuses on both sides instead of sealing under nothing
	z := h
	z.Zero()
	if _, err := Seal(z, now, "t-0042", 1, pt); !errors.Is(err, ErrDEKCold) {
		t.Errorf("zeroed seal: err = %v, want ErrDEKCold", err)
	}
	if _, err := Open(z, now, "t-0042", 123, env); !errors.Is(err, ErrDEKCold) {
		t.Errorf("zeroed open: err = %v, want ErrDEKCold", err)
	}
	// interop: Tink itself, from the same keyset, opens our ciphertext with our AAD (IV embedded, 12 + n + 16 bytes)
	other, err := aead.New(kh)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := other.Decrypt(env.Ciphertext, aad("t-0042", 123, "t-0042/1")); err != nil || !bytes.Equal(got, pt) {
		t.Errorf("tink decrypt of our envelope: %q, %v", got, err)
	}
	if len(env.Ciphertext) != 12+len(pt)+16 {
		t.Errorf("ciphertext length = %d, want %d (IV || ct || tag)", len(env.Ciphertext), 12+len(pt)+16)
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

// latentKMS counts KMS round trips and moves the clock by d before delegating, so a reply lands d after its send.
type latentKMS struct {
	kms.KMS
	clk   *fakeClock
	d     time.Duration
	calls atomic.Int64
}

func (l *latentKMS) KEK(kekID string) (tink.AEADWithContext, error) {
	kek, err := l.KMS.KEK(kekID)
	if err != nil {
		return nil, err
	}
	return &latentKEK{l: l, kek: kek}, nil
}

type latentKEK struct {
	l   *latentKMS
	kek tink.AEADWithContext
}

func (k *latentKEK) EncryptWithContext(ctx context.Context, pt, ad []byte) ([]byte, error) {
	k.l.calls.Add(1)
	k.l.clk.Advance(k.l.d)
	return k.kek.EncryptWithContext(ctx, pt, ad)
}

func (k *latentKEK) DecryptWithContext(ctx context.Context, ct, ad []byte) ([]byte, error) {
	k.l.calls.Add(1)
	k.l.clk.Advance(k.l.d)
	return k.kek.DecryptWithContext(ctx, ct, ad)
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

func newRig(t *testing.T) *rig { return newRigWith(t, nil) }

// newRigWith builds the rig with mod applied to its Config first (nil for the defaults).
func newRigWith(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	fake := kms.NewFake(kms.FakeConfig{Providers: []string{"gcp"}, KEKs: []kms.KEKSpec{{ID: rigKEK, Provider: "gcp", Idx: 0}}, Clock: clk, Rand: rand.New(rand.NewSource(1)), Lock: &sync.Mutex{}})
	lat := &latentKMS{KMS: fake, clk: clk}
	rec := &recorder{}
	store := &mapStore{rec: rec}
	cfg := Config{
		Tenants: []TenantSpec{{ID: rigTenant, Provider: "gcp", KEKID: rigKEK}}, Providers: []string{"gcp"},
		Keys: lat, Store: store, Audit: rec, Clock: clk, Jitter: fixed{0.5}, Spawn: func(f func()) { f() },
		Lease: 30 * time.Second, SoftTTL: 15 * time.Second, EarlyExpiry: time.Second, KMSTimeout: 500 * time.Millisecond,
		BackoffMin: 500 * time.Millisecond, BackoffMax: 8 * time.Second, BackoffJitter: 0.5, RevokedReprobe: 5 * time.Second,
		DEKMaxMessages: 10000, DEKMaxAge: 10 * time.Minute, SweepInterval: 250 * time.Millisecond,
		ProviderInflight: 32, TenantInflight: 2, IngestWaiters: 4,
	}
	if mod != nil {
		mod(&cfg)
	}
	m := New(context.Background(), cfg)
	return &rig{m: m, fake: fake, lat: lat, rec: rec, store: store, clk: clk, t0: clk.Now()}
}

// gatedKMS holds every KEK call open until release is closed and counts how many are inside at once.
type gatedKMS struct {
	kms.KMS
	release chan struct{}
	inside  atomic.Int64
	peak    atomic.Int64
}

func (g *gatedKMS) KEK(kekID string) (tink.AEADWithContext, error) {
	kek, err := g.KMS.KEK(kekID)
	if err != nil {
		return nil, err
	}
	return &gatedKEK{g: g, kek: kek}, nil
}

type gatedKEK struct {
	g   *gatedKMS
	kek tink.AEADWithContext
}

func (k *gatedKEK) enter() {
	n := k.g.inside.Add(1)
	for {
		p := k.g.peak.Load()
		if n <= p || k.g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-k.g.release
	k.g.inside.Add(-1)
}

func (k *gatedKEK) EncryptWithContext(ctx context.Context, pt, ad []byte) ([]byte, error) {
	k.enter()
	return k.kek.EncryptWithContext(ctx, pt, ad)
}

func (k *gatedKEK) DecryptWithContext(ctx context.Context, ct, ad []byte) ([]byte, error) {
	k.enter()
	return k.kek.DecryptWithContext(ctx, ct, ad)
}

func (r *rig) calls() int64 { return r.lat.calls.Load() }

func (r *rig) state() State { return r.m.Info(rigTenant).State }

// TestWrappedDEK: the deks row is a Tink keyset encrypted under the tenant's KEK. It opens only under that KEK with
// the KEK id as associated data, parses as a one-key AES-256-GCM keyset whose primitive decrypts what the handle
// sealed, fails under another KEK with AccessDenied, and, once the KEK is revoked, the same unwrap through Tink's
// helper classifies Deny (the text path CodeFromText covers).
func TestWrappedDEK(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	h, err := r.m.EncryptKey(ctx, rigTenant)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Seal(h, r.clk.Now(), rigTenant, 7, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.store.puts) != 1 {
		t.Fatalf("puts = %d", len(r.store.puts))
	}
	w := r.store.puts[0]
	if w.KEKID != rigKEK || w.KEKVersion != 1 || w.ID != h.DEKID {
		t.Errorf("wrapped dek = %+v", w)
	}
	kek, err := r.fake.KEK(rigKEK)
	if err != nil {
		t.Fatal(err)
	}
	kh, err := keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(w.Wrapped)), kek, []byte(rigKEK))
	if err != nil {
		t.Fatalf("unwrap under own KEK: %v", err)
	}
	if n := kh.Len(); n != 1 {
		t.Errorf("keyset has %d keys, want 1", n)
	}
	if e, err := kh.Primary(); err != nil || e.Key() == nil {
		t.Errorf("primary: %v", err)
	}
	prim, err := aead.New(kh)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := prim.Decrypt(env.Ciphertext, aad(rigTenant, 7, h.DEKID)); err != nil || string(got) != "payload" {
		t.Errorf("unwrapped keyset does not open the envelope: %q, %v", got, err)
	}
	if _, err := keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(w.Wrapped)), kek, []byte("kek-other")); Classify(err) != Deny {
		t.Errorf("other associated data: class %v, err %v", Classify(err), err)
	}
	other := kms.NewFake(kms.FakeConfig{Providers: []string{"aws"}, KEKs: []kms.KEKSpec{{ID: "kek-other", Provider: "aws"}}, Clock: r.clk, Rand: rand.New(rand.NewSource(2)), Lock: &sync.Mutex{}})
	okek, _ := other.KEK("kek-other")
	if _, err := keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(w.Wrapped)), okek, []byte(rigKEK)); Classify(err) != Deny {
		t.Errorf("another KEK: class %v, err %v", Classify(err), err)
	} else if c, _ := kms.CodeFromText(err); c != kms.AccessDenied {
		t.Errorf("another KEK: code %v, err %v", c, err)
	}
	r.fake.Revoke(rigKEK)
	_, err = keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(w.Wrapped)), kek, []byte(rigKEK))
	if Classify(err) != Deny {
		t.Errorf("revoked: class %v, err %v", Classify(err), err)
	}
	if c, _ := kms.CodeFromText(err); c != kms.KeyDisabled {
		t.Errorf("revoked: code %v, err %v", c, err)
	}
	// the Manager's own denied call audits the trimmed cause, not Tink's prefix (the timeline shows Detail verbatim)
	r.clk.Advance(16 * time.Second)
	r.m.EncryptKey(ctx, rigTenant) // hot path: the handle is copied, then the soft-due renewal runs inline and is denied
	if r.state() != Revoked {
		t.Fatalf("state after the denied renewal = %v, want REVOKED", r.state())
	}
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	denied := 0
	for _, e := range r.rec.entries {
		if e.Op == "unwrap" && e.Outcome == "denied" {
			denied++
			if want := "kms gcp: KeyDisabled: " + rigKEK + " is disabled"; e.Detail != want {
				t.Errorf("denied audit Detail = %q, want %q", e.Detail, want)
			}
		}
	}
	if denied != 1 {
		t.Errorf("denied audits = %d, want 1", denied)
	}
}

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
		{"tink-flattened deny", errors.New("keyset.Handle: decryption failed: kms gcp: KeyDisabled: kek is disabled"), Deny},
		{"tink-flattened access denied", errors.New("keyset.Handle: keyset.Handle: encryption failed: kms aws: AccessDenied: no"), Deny},
		{"tink-flattened unavailable", errors.New("keyset.Handle: decryption failed: kms gcp: Unavailable: injected fault"), Transient},
		{"tink-flattened deadline", errors.New("keyset.Handle: decryption failed: context deadline exceeded"), Transient},
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

// TestManagerExtras: the rows M2 › Tests had carried to M5, pulled in at review (PR #6): DEK rotation on the encrypt
// path, ageing of a non-active DEK and the warm path with its backoff cadence, the per-tenant in-flight cap, and the
// stale-OK guard. White-box where the row says so (package cmek).
func TestManagerExtras(t *testing.T) {
	r := newRig(t)
	m, clk := r.m, r.clk
	ctx := context.Background()
	tt := m.tenants[rigTenant]
	expect := func(row string, cond bool, msg string, args ...any) {
		t.Helper()
		if !cond {
			t.Errorf("%s: "+msg, append([]any{row}, args...)...)
		}
	}

	// rotation: with DEKMaxMessages 3 the fourth seal handle comes from a fresh DEK "<t>/2"; the old one stays hot
	m.cfg.DEKMaxMessages = 3
	var h Handle
	for i := 0; i < 3; i++ {
		var err error
		if h, err = m.EncryptKey(ctx, rigTenant); err != nil {
			t.Fatalf("rotation: EncryptKey %d: %v", i, err)
		}
	}
	old := h.DEKID
	expect("rotation", old == rigTenant+"/1" && r.calls() == 1, "first DEK %q after %d calls", old, r.calls())
	h, err := m.EncryptKey(ctx, rigTenant)
	if err != nil {
		t.Fatalf("rotation: fourth EncryptKey: %v", err)
	}
	expect("rotation", h.DEKID == rigTenant+"/2", "fourth handle DEK = %q, want %s/2", h.DEKID, rigTenant)
	expect("rotation", r.calls() == 2 && len(r.store.puts) == 2, "calls %d, puts %d, want 2 and 2", r.calls(), len(r.store.puts))
	if _, err := m.DecryptKey(rigTenant, old); err != nil {
		t.Errorf("rotation: old DEK not decryptable after rotation: %v", err)
	}
	expect("rotation", m.Info(rigTenant).DEKs == 2 && m.Info(rigTenant).HotDEKs == 2, "info = %+v", m.Info(rigTenant))

	// ageing: a non-active DEK's plaintext is dropped at hotSince + DEKMaxAge + Lease; the active one never by age
	tt.mu.Lock()
	tt.deks[old].hotSince = clk.Now().Add(-(m.cfg.DEKMaxAge + m.cfg.Lease))
	tt.mu.Unlock()
	m.Tick(clk.Now())
	expect("ageing", r.rec.count("purge", "aged") == 1, "aged purges = %d", r.rec.count("purge", "aged"))
	if _, err := m.DecryptKey(rigTenant, old); !errors.Is(err, ErrDEKCold) {
		t.Errorf("ageing: DecryptKey(old) err = %v, want ErrDEKCold", err)
	}
	if _, err := m.DecryptKey(rigTenant, h.DEKID); err != nil {
		t.Errorf("ageing: active DEK dropped: %v", err)
	}

	// warm, healthy: Warm makes Hot false until the inline unwrap lands, then the old DEK decrypts again
	calls := r.calls()
	m.Warm(rigTenant, old)
	expect("warm", r.calls() == calls+1, "calls = %d, want %d", r.calls(), calls+1)
	expect("warm", m.Hot(rigTenant) && m.Info(rigTenant).Pending == 0, "Hot = %v, pending = %d after a warm", m.Hot(rigTenant), m.Info(rigTenant).Pending)
	if _, err := m.DecryptKey(rigTenant, old); err != nil {
		t.Errorf("warm: DecryptKey(old) err = %v", err)
	}
	m.Warm(rigTenant, "other/9") // unknown dek id: ignored
	expect("warm", m.Info(rigTenant).Pending == 0, "unknown dek id was recorded as pending")

	// warm under fast-fail: one call per backoff interval, driven by Tick, Hot false throughout [SC-F5]
	tt.mu.Lock()
	tt.deks[old].hotSince = clk.Now().Add(-(m.cfg.DEKMaxAge + m.cfg.Lease))
	tt.mu.Unlock()
	m.Tick(clk.Now())
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{Mode: kms.ModeFastFail})
	calls = r.calls()
	m.Warm(rigTenant, old)
	T := clk.Now()
	expect("warm/fault", r.calls() == calls+1, "calls = %d, want %d (the first warm attempt)", r.calls(), calls+1)
	expect("warm/fault", !m.Hot(rigTenant) && m.Info(rigTenant).Pending == 1, "Hot = %v, pending = %d", m.Hot(rigTenant), m.Info(rigTenant).Pending)
	expect("warm/fault", r.state() == RidingThrough, "state = %v (a failed warm counts as a failed renewal)", r.state())
	// while RIDING_THROUGH, Tick probes the active DEK on the backoff schedule (T+0.5, T+1.5, then T+3.5) and the
	// pending warm waits: the probe heals the state, the warm follows on the next sweep once ACTIVE again
	retryAt := map[time.Duration]bool{500 * time.Millisecond: true, 1500 * time.Millisecond: true}
	want := r.calls()
	for i := 1; i <= 6; i++ { // through T+1.5 s
		clk.Advance(250 * time.Millisecond)
		m.Tick(clk.Now())
		if retryAt[clk.Now().Sub(T)] {
			want++
		}
		expect("warm/fault", r.calls() == want, "at T+%v calls = %d, want %d", clk.Now().Sub(T), r.calls(), want)
		expect("warm/fault", !m.Hot(rigTenant), "Hot = true at T+%v while the DEK is pending", clk.Now().Sub(T))
	}
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{})
	for clk.Now().Sub(T) < 3500*time.Millisecond { // the probe at T+3.5 s succeeds: ACTIVE again
		clk.Advance(250 * time.Millisecond)
		m.Tick(clk.Now())
	}
	expect("warm/fault", r.calls() == want+1 && r.state() == Active, "at T+3.5 s calls = %d (want %d), state = %v", r.calls(), want+1, r.state())
	expect("warm/fault", !m.Hot(rigTenant) && m.Info(rigTenant).Pending == 1, "Hot = %v, pending = %d before the warm retry", m.Hot(rigTenant), m.Info(rigTenant).Pending)
	clk.Advance(250 * time.Millisecond)
	m.Tick(clk.Now()) // the due warm runs now that the tenant is ACTIVE
	expect("warm/fault", r.calls() == want+2, "calls = %d, want %d", r.calls(), want+2)
	expect("warm/fault", m.Hot(rigTenant) && m.Info(rigTenant).Pending == 0 && r.state() == Active, "Hot = %v, pending = %d, state = %v", m.Hot(rigTenant), m.Info(rigTenant).Pending, r.state())
	if _, err := m.DecryptKey(rigTenant, old); err != nil {
		t.Errorf("warm/fault: DecryptKey(old) err = %v", err)
	}

	// in-flight cap: a third concurrent call is errBusy, unclassified, never audited, never a KMS call
	audits, calls := r.rec.len(), r.calls()
	tt.mu.Lock()
	tt.inflight = m.cfg.TenantInflight
	tt.mu.Unlock()
	res := m.call(tt, "unwrap", tt.active)
	tt.mu.Lock()
	tt.inflight = 0
	tt.mu.Unlock()
	expect("inflight", errors.Is(res.err, errBusy), "err = %v, want errBusy", res.err)
	expect("inflight", r.rec.len() == audits && r.calls() == calls, "audits %d→%d, calls %d→%d", audits, r.rec.len(), calls, r.calls())
	m.apply(tt, "warm", tt.deks[old], res) // the errBusy row: no state change, pending re-armed for the next sweep
	expect("inflight", r.rec.len() == audits && r.state() == Active && m.Info(rigTenant).Pending == 1, "after apply: audits %d, state %v, pending %d", r.rec.len(), r.state(), m.Info(rigTenant).Pending)
	tt.mu.Lock()
	delete(tt.pending, old)
	tt.mu.Unlock()

	// stale OK: an OK sent before the latest deny must not un-park a revoked tenant
	r.fake.Revoke(rigKEK)
	clk.Advance(16 * time.Second)
	if _, err := m.EncryptKey(ctx, rigTenant); err != nil { // handle copied, then the kicked probe is denied
		t.Fatalf("stale: EncryptKey: %v", err)
	}
	expect("stale", r.state() == Revoked, "state = %v", r.state())
	tt.mu.Lock()
	deniedAt, active := tt.deniedAt, tt.active
	tt.mu.Unlock()
	m.apply(tt, "unwrap", active, result{sentAt: deniedAt.Add(-time.Second), class: OK})
	expect("stale", r.state() == Revoked && r.rec.count("unwrap", "stale ok ignored") == 1, "state = %v, stale audits = %d", r.state(), r.rec.count("unwrap", "stale ok ignored"))
	expect("stale", m.Info(rigTenant).HotDEKs == 0, "a stale OK installed plaintext")
}

// TestConcurrentColdCallers runs fifty cold-path callers and scheduler visits against one tenant with real
// goroutines: exactly one DEK is generated, and no kicked probe leaves probing set (a joined singleflight call
// clears it after sf.Do returns, not inside the flight).
func TestConcurrentColdCallers(t *testing.T) {
	r := newRig(t)
	r.m.cfg.Spawn = func(f func()) { go f() }
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.m.EncryptKey(ctx, rigTenant)
			r.m.Hot(rigTenant)
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for r.m.Info(rigTenant).Probing && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	info := r.m.Info(rigTenant)
	if info.Probing {
		t.Error("probing still set after every kicked probe returned")
	}
	if info.DEKs != 1 || r.calls() != 1 || len(r.store.puts) != 1 {
		t.Errorf("DEKs %d, calls %d, puts %d; want exactly one generate", info.DEKs, r.calls(), len(r.store.puts))
	}
	if !r.m.Hot(rigTenant) || info.State != Active {
		t.Errorf("Hot = %v, state = %v", r.m.Hot(rigTenant), info.State)
	}
}

// TestPassThrough: the no-cache demo. With pass-through on, every EncryptKey and DecryptKey is one KMS call and the
// cache stays empty; a failed call parks the tenant at once (KEY_UNAVAILABLE, never RIDING_THROUGH) with the
// no-cache detail; recovery and revocation follow the normal probe schedule; switching pass-through off lets the
// cache serve again with no call.
func TestPassThrough(t *testing.T) {
	r := newRig(t)
	m, clk := r.m, r.clk
	ctx := context.Background()
	pt := []byte("payload")
	if _, err := m.EncryptKey(ctx, rigTenant); err != nil { // the cached design: one generate, one hot DEK
		t.Fatal(err)
	}
	if r.calls() != 1 || m.Info(rigTenant).HotDEKs != 1 {
		t.Fatalf("warm-up: calls %d, hot %d", r.calls(), m.Info(rigTenant).HotDEKs)
	}
	m.SetPassThrough(rigTenant, true)
	m.SetPassThrough(rigTenant, true) // idempotent
	if r.rec.count("purge", "cache off") != 1 || m.Info(rigTenant).HotDEKs != 0 {
		t.Fatalf("entering pass-through: purge audits %d, hot %d", r.rec.count("purge", "cache off"), m.Info(rigTenant).HotDEKs)
	}
	var env Envelope
	for i := int64(1); i <= 3; i++ { // three seals, three calls, nothing cached
		h, err := m.EncryptKey(ctx, rigTenant)
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		e, err := Seal(h, clk.Now(), rigTenant, i, pt)
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		env = e
		if r.calls() != 1+i {
			t.Errorf("seal %d: calls = %d, want %d", i, r.calls(), 1+i)
		}
	}
	if m.Info(rigTenant).HotDEKs != 0 || r.rec.count("unwrap", "ok") != 3 {
		t.Errorf("after three seals: hot %d, unwrap ok audits %d", m.Info(rigTenant).HotDEKs, r.rec.count("unwrap", "ok"))
	}
	h, err := m.DecryptKey(rigTenant, env.DEKID) // one call per open
	if err != nil || r.calls() != 5 {
		t.Fatalf("decrypt: err %v, calls %d", err, r.calls())
	}
	if got, err := Open(h, clk.Now(), rigTenant, 3, env); err != nil || !bytes.Equal(got, pt) {
		t.Errorf("open through pass-through: %q, %v", got, err)
	}
	// a transient failure parks the tenant at once: no lease to ride through on
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{Mode: kms.ModeFastFail})
	if _, err := m.EncryptKey(ctx, rigTenant); !errors.Is(err, ErrKeyUnavailable) {
		t.Errorf("fast-fail seal: err = %v, want ErrKeyUnavailable", err)
	}
	if r.state() != KeyUnavailable || len(r.rec.states(RidingThrough)) != 0 {
		t.Errorf("after the failed call: state %v, ride-through lines %d", r.state(), len(r.rec.states(RidingThrough)))
	}
	if st := r.rec.states(KeyUnavailable); len(st) != 1 || st[0].Detail != "no cache: call failed" {
		t.Errorf("KEY_UNAVAILABLE line = %+v", st)
	}
	c := r.calls() // parked: no call per request while parked
	if _, err := m.EncryptKey(ctx, rigTenant); !errors.Is(err, ErrKeyUnavailable) || r.calls() != c {
		t.Errorf("parked seal: err %v, calls %d → %d", err, c, r.calls())
	}
	if _, err := m.DecryptKey(rigTenant, env.DEKID); !errors.Is(err, ErrLeaseExpired) || r.calls() != c {
		t.Errorf("parked open: err %v, calls %d → %d", err, c, r.calls())
	}
	if m.Hot(rigTenant) {
		t.Error("a parked pass-through tenant is Hot")
	}
	// the fault clears: the backoff probe (0.5 s) restores ACTIVE with the cache still empty
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{})
	m.Tick(clk.Now())
	if r.state() != KeyUnavailable {
		t.Error("probed before the backoff")
	}
	clk.Advance(time.Second)
	m.Tick(clk.Now())
	if r.state() != Active || m.Info(rigTenant).HotDEKs != 0 || r.calls() != c+1 {
		t.Errorf("after the probe: state %v, hot %d, calls %d (want %d)", r.state(), m.Info(rigTenant).HotDEKs, r.calls(), c+1)
	}
	if !m.Hot(rigTenant) {
		t.Error("an ACTIVE pass-through tenant is not Hot")
	}
	// a deny is a deny: REVOKED at once, nothing to purge, the Revoke reprobe restores
	r.fake.Revoke(rigKEK)
	if _, err := m.EncryptKey(ctx, rigTenant); !errors.Is(err, ErrKeyRevoked) {
		t.Errorf("revoked seal: err = %v, want ErrKeyRevoked", err)
	}
	if st := r.rec.states(Revoked); len(st) != 1 || st[0].Purged != 0 || r.state() != Revoked {
		t.Errorf("REVOKED: lines %+v, state %v", st, r.state())
	}
	r.fake.Restore(rigKEK)
	clk.Advance(5 * time.Second)
	m.Tick(clk.Now())
	if r.state() != Active {
		t.Errorf("after restore and reprobe: state %v", r.state())
	}
	// pass-through off: the next seal fills the cache with one call, the one after is served without a call
	m.SetPassThrough(rigTenant, false)
	c = r.calls()
	if _, err := m.EncryptKey(ctx, rigTenant); err != nil || r.calls() != c+1 || m.Info(rigTenant).HotDEKs != 1 {
		t.Errorf("first seal with the cache back: err %v, calls %d (want %d), hot %d", err, r.calls(), c+1, m.Info(rigTenant).HotDEKs)
	}
	if _, err := m.EncryptKey(ctx, rigTenant); err != nil || r.calls() != c+1 {
		t.Errorf("second seal with the cache back: err %v, calls %d (want %d)", err, r.calls(), c+1)
	}
	// concurrent first events on a DEK-less pass-through tenant generate one DEK (the generate flight). A caller
	// either seals with it or, arriving after the flight and finding two calls already in flight for the tenant
	// (TenantInflight = 2 in the rig), gets the cap's ErrKeyUnavailable: per-request calls run into the cap, as the
	// card says. Nothing else may fail, and at least one caller seals.
	r2 := newRig(t)
	r2.m.SetPassThrough(rigTenant, true)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int64) {
			defer wg.Done()
			h, err := r2.m.EncryptKey(ctx, rigTenant)
			if err == nil {
				_, err = Seal(h, r2.clk.Now(), rigTenant, i, pt)
			}
			errs <- err
		}(int64(i))
	}
	wg.Wait()
	close(errs)
	sealed := 0
	for err := range errs {
		switch {
		case err == nil:
			sealed++
		case errors.Is(err, ErrKeyUnavailable): // the per-tenant in-flight cap
		default:
			t.Errorf("concurrent first event: %v", err)
		}
	}
	if sealed == 0 {
		t.Error("no concurrent first event sealed")
	}
	if len(r2.store.puts) != 1 || r2.rec.count("generate", "ok") != 1 || r2.m.Info(rigTenant).DEKs != 1 {
		t.Errorf("concurrent first events: puts %d, generate audits %d, DEKs %d; want one of each", len(r2.store.puts), r2.rec.count("generate", "ok"), r2.m.Info(rigTenant).DEKs)
	}
}

// TestNaiveSkipsBulkheads: a naive tenant's calls pass neither the per-tenant cap nor the provider semaphore (six
// concurrent seals are all inside the KMS at once, and in flight reads six), while the same tenant back on the cached
// design sends exactly one call for the same six seals (the cold fetch is a singleflight; the caps never even bind).
func TestNaiveSkipsBulkheads(t *testing.T) {
	gate := &gatedKMS{release: make(chan struct{})}
	r := newRigWith(t, func(c *Config) {
		gate.KMS = c.Keys
		c.Keys = gate
		c.ProviderInflight, c.TenantInflight = 2, 2
	})
	m, ctx := r.m, context.Background()
	// warm-up with the gate open: one generate, so pass-through seals are direct unwraps outside singleflight
	close(gate.release)
	if _, err := m.EncryptKey(ctx, rigTenant); err != nil {
		t.Fatal(err)
	}
	gate.release = make(chan struct{})
	gate.peak.Store(0)
	m.SetNaive(rigTenant, true)
	if m.Info(rigTenant).HotDEKs != 0 {
		t.Fatalf("naive: hot %d", m.Info(rigTenant).HotDEKs)
	}
	const n = 6
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := m.EncryptKey(ctx, rigTenant)
			errs <- err
		}()
	}
	for i := 0; i < 200 && gate.inside.Load() < n; i++ { // every caller reaches the KMS: no cap held any back
		time.Sleep(time.Millisecond)
	}
	if got := gate.inside.Load(); got != n {
		t.Errorf("naive: %d of %d calls inside the KMS at once", got, n)
	}
	if got := m.Inflight("gcp"); got != n {
		t.Errorf("naive: in flight reads %d, want %d", got, n)
	}
	close(gate.release)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("naive seal: %v", err)
		}
	}
	// the cached design again: six concurrent seals on an empty cache share one cold fetch; exactly one call is inside
	m.SetNaive(rigTenant, false)
	gate.release = make(chan struct{})
	gate.peak.Store(0)
	for i := 0; i < n; i++ {
		go func() {
			_, err := m.EncryptKey(ctx, rigTenant)
			errs <- err
		}()
	}
	for i := 0; i < 200 && gate.inside.Load() < 1; i++ {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // any second caller would have arrived by now
	if got := gate.peak.Load(); got != 1 {
		t.Errorf("leased: %d calls inside the KMS at once, want the one shared cold fetch", got)
	}
	close(gate.release)
	sealed := 0
	for i := 0; i < n; i++ {
		switch err := <-errs; {
		case err == nil:
			sealed++
		case errors.Is(err, ErrKeyUnavailable): // beyond IngestWaiters: the fast 503
		default:
			t.Errorf("leased seal: %v", err)
		}
	}
	if sealed == 0 || r.calls() != 1+n+1 {
		t.Errorf("leased: %d sealed, %d KMS calls in all (want warm-up 1 + naive %d + one shared fetch)", sealed, r.calls(), n)
	}
}

// TestSyncFetch: in sync-fetch mode nothing is kicked into the background. A soft-due lease is renewed by the
// worker's DecryptKey itself (Hot and EncryptKey serve from the cache without a call), a failed inline renewal still
// rides through on the usable lease, a lapsed tenant with a due probe is Hot so a worker gets dispatched to probe
// it, and that probe heals it. Back in async mode the same soft-due lease is kicked from Hot.
func TestSyncFetch(t *testing.T) {
	r := newRig(t)
	m, clk, ctx := r.m, r.clk, context.Background()
	h, err := m.EncryptKey(ctx, rigTenant) // async warm-up: one generate
	if err != nil {
		t.Fatal(err)
	}
	dekID := h.DEKID
	m.SetFetch(rigTenant, FetchSync)
	if m.Info(rigTenant).HotDEKs != 1 || r.calls() != 1 {
		t.Fatalf("entering sync: hot %d, calls %d (the cache is kept)", m.Info(rigTenant).HotDEKs, r.calls())
	}
	clk.Advance(16 * time.Second) // soft-due (SoftTTL 15 s), still usable
	if !m.Hot(rigTenant) || r.calls() != 1 || m.Info(rigTenant).Probing {
		t.Errorf("sync Hot: hot %v, calls %d, probing %v (nothing may be kicked)", m.Hot(rigTenant), r.calls(), m.Info(rigTenant).Probing)
	}
	if _, err := m.EncryptKey(ctx, rigTenant); err != nil || r.calls() != 1 {
		t.Errorf("sync ingest hot path: err %v, calls %d (served from the cache, no kick)", err, r.calls())
	}
	if _, err := m.DecryptKey(rigTenant, dekID); err != nil || r.calls() != 2 || r.rec.count("unwrap", "ok") != 1 {
		t.Errorf("sync worker: err %v, calls %d, unwrap ok %d (the worker renews inline)", err, r.calls(), r.rec.count("unwrap", "ok"))
	}
	if age := m.Info(rigTenant).LeaseAge; age != 0 {
		t.Errorf("lease age after the inline renewal: %s, want 0", age)
	}
	if _, err := m.DecryptKey(rigTenant, dekID); err != nil || r.calls() != 2 {
		t.Errorf("second worker call: err %v, calls %d (the lease is fresh: no call)", err, r.calls())
	}
	// the inline renewal fails: RIDING_THROUGH on the usable lease, the handle still served
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{Mode: kms.ModeFastFail})
	clk.Advance(16 * time.Second)
	if _, err := m.DecryptKey(rigTenant, dekID); err != nil || r.calls() != 3 || r.state() != RidingThrough {
		t.Errorf("failed inline renewal: err %v, calls %d, state %v", err, r.calls(), r.state())
	}
	// the lease lapses: Tick parks the tenant; Tick never probes a sync tenant, but Hot dispatches it once the probe is due
	clk.Advance(15 * time.Second)
	m.Tick(clk.Now())
	if r.state() != KeyUnavailable || r.calls() != 3 {
		t.Fatalf("after the lapse: state %v, calls %d (Tick must not probe)", r.state(), r.calls())
	}
	if !m.Hot(rigTenant) || r.calls() != 3 {
		t.Errorf("parked sync tenant with a due probe: hot %v, calls %d (dispatched, not kicked)", m.Hot(rigTenant), r.calls())
	}
	r.fake.SetFault(kms.Scope{Provider: "gcp"}, kms.Fault{})
	if _, err := m.DecryptKey(rigTenant, dekID); err != nil || r.state() != Active || r.calls() != 4 || m.Info(rigTenant).HotDEKs != 1 {
		t.Errorf("worker probe heals: err %v, state %v, calls %d, hot %d", err, r.state(), r.calls(), m.Info(rigTenant).HotDEKs)
	}
	// async again: the soft-due lease is kicked from Hot (Spawn runs it inline in the rig)
	m.SetFetch(rigTenant, FetchAsync)
	clk.Advance(16 * time.Second)
	if !m.Hot(rigTenant) || r.calls() != 5 {
		t.Errorf("async Hot: hot %v, calls %d (the kick)", m.Hot(rigTenant), r.calls())
	}
}
