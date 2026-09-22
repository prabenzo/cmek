// Owner: Claude
package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	"math"
	mrand "math/rand"
	"sync"
	"time"
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
	key      [32]byte
	provider string
	idx      int
	enabled  bool
}

// Fake is three in-process providers doing real AES-256-GCM wrapping under per-KEK keys, with fault injection per
// provider or per KEK (fast-fail, error rate, lognormal latency), Revoke/Restore and a ground-truth log.
type Fake struct {
	cfg        FakeConfig
	mu         sync.RWMutex
	keks       map[string]*kek
	provFaults map[string]Fault
	kekFaults  map[string]Fault
	truth      Truth
}

// NewFake builds the providers; every KEK gets 32 bytes from crypto/rand.
func NewFake(cfg FakeConfig) *Fake {
	f := &Fake{cfg: cfg, keks: make(map[string]*kek, len(cfg.KEKs)), provFaults: make(map[string]Fault), kekFaults: make(map[string]Fault)}
	for _, s := range cfg.KEKs {
		k := &kek{provider: s.Provider, idx: s.Idx, enabled: true}
		if _, err := io.ReadFull(rand.Reader, k.key[:]); err != nil {
			panic("kms: crypto/rand: " + err.Error())
		}
		f.keks[s.ID] = k
	}
	return f
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
// section, so no call that returned OK was checked after it [SC-F6]. Restore re-enables it the same way.
func (f *Fake) Revoke(kekID string)  { f.flip(kekID, false) }
func (f *Fake) Restore(kekID string) { f.flip(kekID, true) }

func (f *Fake) flip(kekID string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keks[kekID]
	if k == nil {
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

func (k *kek) aead() cipher.AEAD {
	block, err := aes.NewCipher(k.key[:])
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
}

// GenerateDataKey returns a fresh 32-byte DEK and its wrapped form (nonce || AES-256-GCM(DEK) under the KEK).
func (f *Fake) GenerateDataKey(ctx context.Context, kekID string) (DataKey, error) {
	k, err := f.begin(ctx, kekID)
	if err != nil {
		return DataKey{}, err
	}
	var dk DataKey
	if _, err := io.ReadFull(rand.Reader, dk.Plaintext[:]); err != nil {
		return DataKey{}, &Error{Code: Unavailable, Provider: k.provider, Msg: err.Error()}
	}
	g := k.aead()
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return DataKey{}, &Error{Code: Unavailable, Provider: k.provider, Msg: err.Error()}
	}
	dk.Wrapped = g.Seal(nonce, nonce, dk.Plaintext[:], []byte(kekID))
	dk.KEKVersion = 1
	return dk, nil
}

// Unwrap recovers a DEK from its wrapped form; a disabled KEK answers KeyDisabled, checked after any latency.
func (f *Fake) Unwrap(ctx context.Context, kekID string, kekVersion int, wrapped []byte) ([32]byte, error) {
	var out [32]byte
	k, err := f.begin(ctx, kekID)
	if err != nil {
		return out, err
	}
	g := k.aead()
	ns := g.NonceSize()
	if len(wrapped) < ns {
		return out, &Error{Code: AccessDenied, Provider: k.provider, Msg: "malformed wrapped key"}
	}
	pt, err := g.Open(nil, wrapped[:ns], wrapped[ns:], []byte(kekID))
	if err != nil || len(pt) != 32 {
		return out, &Error{Code: AccessDenied, Provider: k.provider, Msg: "wrapped key does not verify"}
	}
	copy(out[:], pt)
	return out, nil
}
