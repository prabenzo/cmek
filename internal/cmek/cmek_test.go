// Owner: Claude (reviewed by Ben)
package cmek

import (
	"bytes"
	"errors"
	"testing"
	"time"
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
