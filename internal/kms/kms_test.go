// Owner: Claude (reviewed by Ben)
package kms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"
)

// TestCodeFromText pins the texts the shim must read: our own frame, the two shapes Tink's keyset helpers wrap it
// in, a flattened context deadline, and non-matches.
func TestCodeFromText(t *testing.T) {
	rows := []struct {
		name string
		err  error
		want Code
		ok   bool
	}{
		{"nil", nil, 0, false},
		{"ours", &Error{Code: KeyDisabled, Provider: "gcp", Msg: "kek-t-0754 is disabled"}, KeyDisabled, true},
		{"tink read", errors.New("keyset.Handle: decryption failed: kms gcp: KeyDisabled: kek-t-0754 is disabled"), KeyDisabled, true},
		{"tink write", errors.New("keyset.Handle: keyset.Handle: encryption failed: kms aws: Unavailable: injected fault"), Unavailable, true},
		{"denied", fmt.Errorf("wrapped: %v", &Error{Code: AccessDenied, Provider: "azure", Msg: "wrapped key does not verify"}), AccessDenied, true},
		{"timeout code", errors.New("kms gcp: Timeout: deadline"), Timeout, true},
		{"flattened deadline", errors.New("keyset.Handle: decryption failed: context deadline exceeded"), Timeout, true},
		{"deadline itself", context.DeadlineExceeded, Timeout, true},
		{"code word without the frame", errors.New("KeyDisabled"), 0, false},
		{"eof", io.EOF, 0, false},
		{"empty", errors.New(""), 0, false},
	}
	for _, r := range rows {
		got, ok := CodeFromText(r.err)
		if got != r.want || ok != r.ok {
			t.Errorf("%s: CodeFromText = %v, %v; want %v, %v", r.name, got, ok, r.want, r.ok)
		}
	}
}

// TestCodeThroughTink runs the real keyset helpers against the fake gate: the typed error is gone (errors.As fails)
// and CodeFromText recovers the code in every case cmek classifies. A Tink upgrade that changes the shape fails here.
func TestCodeThroughTink(t *testing.T) {
	clk := &manualClock{now: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), ch: make(chan time.Time, 1), armed: make(chan struct{}, 1)}
	f := newTestFake(clk)
	ctx := context.Background()
	a, _ := f.KEK("kek-a")
	b, _ := f.KEK("kek-b")
	h, err := keyset.NewHandle(aead.AES256GCMNoPrefixKeyTemplate())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := h.WriteWithContext(ctx, keyset.NewBinaryWriter(&buf), a, []byte("kek-a")); err != nil {
		t.Fatalf("wrap: %v", err)
	}
	wrapped := buf.Bytes()
	read := func(ctx context.Context, kek interface {
		DecryptWithContext(context.Context, []byte, []byte) ([]byte, error)
		EncryptWithContext(context.Context, []byte, []byte) ([]byte, error)
	}) error {
		_, err := keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(wrapped)), kek, []byte("kek-a"))
		return err
	}
	if err := read(ctx, a); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	f.Revoke("kek-a")
	disabled := read(ctx, a)
	f.Restore("kek-a")
	f.SetFault(Scope{Provider: "gcp"}, Fault{Mode: ModeFastFail})
	fastFail := h.WriteWithContext(ctx, keyset.NewBinaryWriter(&bytes.Buffer{}), a, []byte("kek-a"))
	f.SetFault(Scope{Provider: "gcp"}, Fault{P50: time.Second, P99: time.Second})
	expired, cancel := context.WithDeadline(ctx, clk.now.Add(-time.Second))
	defer cancel()
	timedOut := read(expired, a)
	f.SetFault(Scope{Provider: "gcp"}, Fault{})
	otherKEK := read(ctx, b)
	rows := []struct {
		name string
		err  error
		want Code
	}{
		{"disabled", disabled, KeyDisabled},
		{"fast-fail on write", fastFail, Unavailable},
		{"deadline", timedOut, Timeout},
		{"another KEK", otherKEK, AccessDenied},
	}
	for _, r := range rows {
		var ke *Error
		if errors.As(r.err, &ke) || errors.Is(r.err, context.DeadlineExceeded) {
			t.Errorf("%s: the typed error survived Tink (%v); the shim is no longer needed", r.name, r.err)
		}
		if got, ok := CodeFromText(r.err); !ok || got != r.want {
			t.Errorf("%s: CodeFromText(%q) = %v, %v; want %v", r.name, r.err, got, ok, r.want)
		}
	}
}
