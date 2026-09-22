// Owner: Claude
package kms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"
	mrand "math/rand"
	"sync"
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

// Fake is three in-process providers doing real AES-256-GCM wrapping under per-KEK keys.
// M1: every key enabled, no faults, no latency; M2 adds faults, latency, Revoke/Restore and ground truth.
type Fake struct {
	cfg  FakeConfig
	mu   sync.RWMutex
	keks map[string]*kek
}

// NewFake builds the providers; every KEK gets 32 bytes from crypto/rand.
func NewFake(cfg FakeConfig) *Fake {
	f := &Fake{cfg: cfg, keks: make(map[string]*kek, len(cfg.KEKs))}
	for _, s := range cfg.KEKs {
		k := &kek{provider: s.Provider, idx: s.Idx, enabled: true}
		if _, err := io.ReadFull(rand.Reader, k.key[:]); err != nil {
			panic("kms: crypto/rand: " + err.Error())
		}
		f.keks[s.ID] = k
	}
	return f
}

func (f *Fake) lookup(kekID string) (*kek, error) {
	f.mu.RLock()
	k := f.keks[kekID]
	f.mu.RUnlock()
	if k == nil {
		return nil, &Error{Code: AccessDenied, Provider: "?", Msg: "unknown key " + kekID}
	}
	return k, nil
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
	k, err := f.lookup(kekID)
	if err != nil {
		return DataKey{}, err
	}
	f.mu.RLock()
	enabled := k.enabled
	f.mu.RUnlock()
	if !enabled {
		return DataKey{}, &Error{Code: KeyDisabled, Provider: k.provider, Msg: kekID + " is disabled"}
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

// Unwrap recovers a DEK from its wrapped form; a disabled KEK answers KeyDisabled (checked after any latency, M2).
func (f *Fake) Unwrap(ctx context.Context, kekID string, kekVersion int, wrapped []byte) ([32]byte, error) {
	var out [32]byte
	k, err := f.lookup(kekID)
	if err != nil {
		return out, err
	}
	f.mu.RLock()
	enabled := k.enabled
	f.mu.RUnlock()
	if !enabled {
		return out, &Error{Code: KeyDisabled, Provider: k.provider, Msg: kekID + " is disabled"}
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
