// Owner: Claude (reviewed by Ben)
package kms

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// manualClock: Now is fixed; After hands out a channel the test fires, and signals `armed` when a caller is waiting.
type manualClock struct {
	now   time.Time
	ch    chan time.Time
	armed chan struct{}
}

func (c *manualClock) Now() time.Time { return c.now }
func (c *manualClock) After(time.Duration) <-chan time.Time {
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return c.ch
}

func newTestFake(clk Clock) *Fake {
	return NewFake(FakeConfig{Providers: []string{"gcp"}, KEKs: []KEKSpec{{ID: "kek-a", Provider: "gcp", Idx: 0}, {ID: "kek-b", Provider: "gcp", Idx: 1}}, Clock: clk, Rand: rand.New(rand.NewSource(1)), Lock: &sync.Mutex{}})
}

func codeOf(t *testing.T, err error) Code {
	t.Helper()
	var ke *Error
	if !errors.As(err, &ke) {
		t.Fatalf("err = %v, want *kms.Error", err)
	}
	return ke.Code
}

// TestGate: the gate keeps every behaviour the scenarios need in front of the Tink primitive: unknown KEK, fast-fail
// before any latency, error rate, latency raced with the ctx, the enabled flag read after the latency, and a
// wrapped blob that verifies only under its own KEK and associated data.
func TestGate(t *testing.T) {
	clk := &manualClock{now: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), ch: make(chan time.Time, 1), armed: make(chan struct{}, 1)}
	f := newTestFake(clk)
	ctx := context.Background()
	if _, err := f.KEK("nope"); codeOf(t, err) != AccessDenied {
		t.Errorf("unknown KEK: %v", err)
	}
	a, err := f.KEK("kek-a")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := f.KEK("kek-b")
	blob := []byte("a one-key keyset would go here")
	wrapped, err := a.EncryptWithContext(ctx, blob, []byte("kek-a"))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if pt, err := a.DecryptWithContext(ctx, wrapped, []byte("kek-a")); err != nil || string(pt) != string(blob) {
		t.Errorf("unwrap: %q, %v", pt, err)
	}
	if _, err := b.DecryptWithContext(ctx, wrapped, []byte("kek-a")); codeOf(t, err) != AccessDenied {
		t.Errorf("unwrap under another KEK: %v", err)
	}
	if _, err := a.DecryptWithContext(ctx, wrapped, []byte("kek-b")); codeOf(t, err) != AccessDenied {
		t.Errorf("unwrap with another associated data: %v", err)
	}

	// fast-fail answers before any latency: After is never consulted
	f.SetFault(Scope{Provider: "gcp"}, Fault{Mode: ModeFastFail, P50: time.Second, P99: time.Second})
	if _, err := a.DecryptWithContext(ctx, wrapped, []byte("kek-a")); codeOf(t, err) != Unavailable {
		t.Errorf("fast-fail: %v", err)
	}
	select {
	case <-clk.armed:
		t.Error("fast-fail waited on the clock")
	default:
	}
	f.SetFault(Scope{Provider: "gcp"}, Fault{ErrorRate: 1})
	if _, err := a.EncryptWithContext(ctx, blob, []byte("kek-a")); codeOf(t, err) != Unavailable {
		t.Errorf("error rate 1: %v", err)
	}

	// latency past the deadline is the ctx error, not a kms.Error
	f.SetFault(Scope{Provider: "gcp"}, Fault{P50: time.Second, P99: time.Second})
	expired, cancel := context.WithDeadline(ctx, clk.now.Add(-time.Second))
	defer cancel()
	if _, err := a.DecryptWithContext(expired, wrapped, []byte("kek-a")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	<-clk.armed

	// the enabled flag is read after the latency: a revoke during the wait turns the call into KeyDisabled
	done := make(chan error, 1)
	go func() {
		_, err := a.DecryptWithContext(ctx, wrapped, []byte("kek-a"))
		done <- err
	}()
	<-clk.armed
	f.Revoke("kek-a")
	clk.ch <- clk.now
	if err := <-done; codeOf(t, err) != KeyDisabled {
		t.Errorf("revoked during latency: %v", err)
	}
	if ev := f.Truth().KeyEvents(); len(ev) != 1 || ev[0].KEKID != "kek-a" || ev[0].Enabled {
		t.Errorf("truth = %+v", ev)
	}
	f.Revoke("kek-a") // already revoked: no second event
	if ev := f.Truth().KeyEvents(); len(ev) != 1 {
		t.Errorf("a repeated revoke recorded an event: %+v", ev)
	}
	f.SetFault(Scope{Provider: "gcp"}, Fault{})
	if _, err := b.EncryptWithContext(ctx, blob, []byte("kek-b")); err != nil {
		t.Errorf("kek-b after kek-a revoked: %v", err)
	}
	f.Restore("kek-a")
	f.Restore("kek-a") // the scenario's exit after a manual Restore: still one enable event
	if ev := f.Truth().KeyEvents(); len(ev) != 2 || !ev[1].Enabled {
		t.Errorf("truth after restore = %+v, want one disable and one enable", ev)
	}
	if _, err := a.DecryptWithContext(ctx, wrapped, []byte("kek-a")); err != nil {
		t.Errorf("after restore: %v", err)
	}
}
