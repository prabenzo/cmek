// Owner: Claude
package kms

import (
	"context"
	"math"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// KEKSpec binds a KEK to a provider and a grid index.
type KEKSpec struct {
	ID, Provider string
	Idx          int
}

// FakeConfig sizes the fake.
type FakeConfig struct {
	Providers []string
	KEKs      []KEKSpec
	Clock     Clock
	Rand      *mrand.Rand
	Lock      *sync.Mutex
}

type kek struct {
	prim     tink.AEAD // the KEK itself: a one-key Tink AES-256-GCM keyset, never serialized
	provider string
	idx      int
	enabled  bool
}

// Fake is three in-process providers doing real AES-256-GCM wrapping (Tink) under per-KEK keys, with fault
// injection per provider or per KEK (fast-fail, error rate, lognormal latency), Revoke/Restore and a ground-truth log.
type Fake struct {
	cfg        FakeConfig
	mu         sync.RWMutex
	keks       map[string]*kek
	provFaults map[string]Fault
	kekFaults  map[string]Fault
	truth      Truth
}

// NewFake builds the providers; every KEK is a fresh Tink AES-256-GCM keyset (crypto/rand inside Tink).
func NewFake(cfg FakeConfig) *Fake {
	f := &Fake{cfg: cfg, keks: make(map[string]*kek, len(cfg.KEKs)), provFaults: make(map[string]Fault), kekFaults: make(map[string]Fault)}
	for _, s := range cfg.KEKs {
		h, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
		if err != nil {
			panic("kms: new keyset: " + err.Error())
		}
		prim, err := aead.New(h)
		if err != nil {
			panic("kms: aead: " + err.Error())
		}
		f.keks[s.ID] = &kek{prim: prim, provider: s.Provider, idx: s.Idx, enabled: true}
	}
	return f
}

// KEK returns the remote AEAD for one KEK: a gate that runs the fault, latency and revoke checks on every call and
// then wraps or unwraps under the KEK's own Tink primitive. The lookup is offline; an unknown id is AccessDenied.
func (f *Fake) KEK(kekID string) (tink.AEADWithContext, error) {
	f.mu.RLock()
	_, ok := f.keks[kekID]
	f.mu.RUnlock()
	if !ok {
		return nil, &Error{Code: AccessDenied, Provider: "?", Msg: "unknown key " + kekID}
	}
	return &gate{f: f, kekID: kekID}, nil
}

// gate is one KEK's tink.AEADWithContext: begin (fault → fast-fail / error rate / latency raced with ctx → enabled
// read after the latency) and then the KEK primitive with the caller's associated data (cmek passes the KEK id).
type gate struct {
	f     *Fake
	kekID string
}

// EncryptWithContext wraps a DEK keyset under the KEK (the generate round trip).
func (g *gate) EncryptWithContext(ctx context.Context, plaintext, associatedData []byte) ([]byte, error) {
	k, err := g.f.begin(ctx, g.kekID)
	if err != nil {
		return nil, err
	}
	ct, err := k.prim.Encrypt(plaintext, associatedData)
	if err != nil {
		return nil, &Error{Code: Unavailable, Provider: k.provider, Msg: err.Error()}
	}
	return ct, nil
}

// DecryptWithContext unwraps a DEK keyset (the unwrap round trip); bytes that do not verify under this KEK and
// this associated data are an authoritative deny, as a real provider answers for a blob wrapped by another key.
func (g *gate) DecryptWithContext(ctx context.Context, ciphertext, associatedData []byte) ([]byte, error) {
	k, err := g.f.begin(ctx, g.kekID)
	if err != nil {
		return nil, err
	}
	pt, err := k.prim.Decrypt(ciphertext, associatedData)
	if err != nil {
		return nil, &Error{Code: AccessDenied, Provider: k.provider, Msg: "wrapped key does not verify"}
	}
	return pt, nil
}

// SetFault installs a fault for a provider or a KEK; a zero Fault (ModeOK, no latency, no error rate) clears it.
func (f *Fake) SetFault(s Scope, fault Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, key := f.provFaults, s.Provider
	if s.KEKID != "" {
		m, key = f.kekFaults, s.KEKID
	}
	if fault.IsZero() {
		delete(m, key)
		return
	}
	m[key] = fault
}

// Revoke disables the KEK and records ground truth; the timestamp is read after the flip, inside the same critical
// section, so no call that returned OK was checked after it [SC-F6]. Restore re-enables it the same way. Both are
// idempotent: a flip to the state the key is already in records nothing (a Restore that ends the revocation
// scenario is followed by the scenario's own exit Restore; the checker must see one event).
func (f *Fake) Revoke(kekID string)  { f.flip(kekID, false) }
func (f *Fake) Restore(kekID string) { f.flip(kekID, true) }

func (f *Fake) flip(kekID string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keks[kekID]
	if k == nil || k.enabled == enabled {
		return
	}
	k.enabled = enabled
	f.truth.append(KeyEvent{KEKID: kekID, Idx: k.idx, At: f.cfg.Clock.Now(), Enabled: enabled})
}

// Truth exposes ground truth; world hands it to the checker only (M5).
func (f *Fake) Truth() *Truth { return &f.truth }

// begin resolves the KEK and its fault, then applies fast-fail, error rate and latency (raced against ctx).
// The enabled flag is read only after the latency, under the read lock, so an OK is always checked strictly
// before any revoke stamped later.
func (f *Fake) begin(ctx context.Context, kekID string) (*kek, error) {
	f.mu.RLock()
	k := f.keks[kekID]
	var fault Fault
	if k != nil {
		fault = resolve(f.provFaults[k.provider], f.kekFaults[kekID])
	}
	f.mu.RUnlock()
	if k == nil {
		return nil, &Error{Code: AccessDenied, Provider: "?", Msg: "unknown key " + kekID}
	}
	if fault.Mode == ModeFastFail || (fault.ErrorRate > 0 && f.uniform() < fault.ErrorRate) {
		return nil, &Error{Code: Unavailable, Provider: k.provider, Msg: "injected fault"}
	}
	if fault.P50 > 0 {
		select {
		case <-f.cfg.Clock.After(f.lognormal(fault.P50, fault.P99)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.RLock()
	enabled := k.enabled
	f.mu.RUnlock()
	if !enabled {
		return nil, &Error{Code: KeyDisabled, Provider: k.provider, Msg: kekID + " is disabled"}
	}
	return k, nil
}

func (f *Fake) uniform() float64 {
	f.cfg.Lock.Lock()
	defer f.cfg.Lock.Unlock()
	return f.cfg.Rand.Float64()
}

// lognormal draws a latency with median p50 and 99th percentile p99: μ = ln(P50), σ = (ln P99 − ln P50) / 2.326.
func (f *Fake) lognormal(p50, p99 time.Duration) time.Duration {
	mu := math.Log(p50.Seconds())
	sigma := 0.0
	if p99 > p50 {
		sigma = (math.Log(p99.Seconds()) - mu) / 2.326
	}
	f.cfg.Lock.Lock()
	z := f.cfg.Rand.NormFloat64()
	f.cfg.Lock.Unlock()
	return time.Duration(math.Exp(mu+sigma*z) * float64(time.Second))
}
