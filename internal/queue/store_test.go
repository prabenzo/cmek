// Owner: Claude (reviewed by Ben)
package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prabenzo/cmek/internal/cmek"
)

type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

func openTest(tb testing.TB, dir string) (*Store, *testClock) {
	tb.Helper()
	clk := &testClock{now: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	s, err := Open(Config{Path: filepath.Join(dir, "t.db"), Tenants: []string{"t-0000", "t-0001"}, Clock: clk})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	return s, clk
}

func env(n byte) cmek.Envelope {
	return cmek.Envelope{DEKID: "t-0000/1", Nonce: [12]byte{n}, Ciphertext: []byte("ciphertext-not-plaintext")}
}

// TestLedger: insert, claim, release, dead, claim again, ack; the ledger must move by exactly the rows each statement changed.
func TestLedger(t *testing.T) {
	ctx := context.Background()
	s, clk := openTest(t, t.TempDir())
	for i := 0; i < 3; i++ {
		if err := s.Insert(ctx, 0, s.NextID(), env(byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	if s.Total() != 3 || s.Backlog(0) != 3 || s.Backlogged() != 1 {
		t.Fatalf("after insert: total=%d backlog=%d backlogged=%d", s.Total(), s.Backlog(0), s.Backlogged())
	}
	b, err := s.Claim(ctx, 0, 2, clk.now.Add(30*time.Second))
	if err != nil || len(b.Msgs) != 2 || b.Msgs[0].ID != 1 || b.Msgs[1].ID != 2 {
		t.Fatalf("claim: %v %+v", err, b)
	}
	if s.Ready(0) != 1 || s.claimed[0].Load() != 2 {
		t.Fatalf("after claim: ready=%d claimed=%d", s.Ready(0), s.claimed[0].Load())
	}
	if n, _ := s.Release(ctx, 0, []int64{1}); n != 1 {
		t.Fatalf("release: %d", n)
	}
	if err := s.Dead(ctx, 0, 2); err != nil {
		t.Fatal(err)
	}
	b, _ = s.Claim(ctx, 0, 2, clk.now.Add(30*time.Second))
	if len(b.Msgs) != 2 || b.Msgs[0].ID != 1 || b.Msgs[1].ID != 3 {
		t.Fatalf("second claim: %+v", b.Msgs)
	}
	if n, _ := s.Ack(ctx, 0, []int64{1, 3}); n != 2 {
		t.Fatalf("ack: %d", n)
	}
	got := [6]int64{s.accepted[0].Load(), s.delivered[0].Load(), s.ready[0].Load(), s.claimed[0].Load(), s.deadN[0].Load(), int64(s.Total())}
	if got != [6]int64{3, 2, 0, 0, 1, 0} {
		t.Fatalf("ledger accepted,delivered,ready,claimed,dead,total = %v", got)
	}
	if s.Backlog(0) != 0 || s.Backlogged() != 0 {
		t.Fatalf("backlog=%d backlogged=%d", s.Backlog(0), s.Backlogged())
	}
	if n, _ := s.Ack(ctx, 0, []int64{1, 3}); n != 0 {
		t.Fatalf("second ack moved %d rows", n)
	}
	// Reclaim: a claim past its deadline goes back to ready
	if err := s.Insert(ctx, 1, s.NextID(), env(9)); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Claim(ctx, 1, 8, clk.now.Add(30*time.Second)); len(b.Msgs) != 1 {
		t.Fatalf("claim tenant 1: %+v", b)
	}
	clk.now = clk.now.Add(31 * time.Second)
	if n, _ := s.Reclaim(ctx, clk.now); n != 1 || s.Ready(1) != 1 || s.claimed[1].Load() != 0 {
		t.Fatalf("reclaim: n=%d ready=%d claimed=%d", n, s.Ready(1), s.claimed[1].Load())
	}
}

// BenchmarkInsert: ns/op of one ~1.1 KB insert on a fresh store (the laptop instrument; the tmpfs run in the session is the decision).
func BenchmarkInsert(b *testing.B) {
	s, _ := openTest(b, b.TempDir())
	e := cmek.Envelope{DEKID: "t-0000/1", Ciphertext: make([]byte, 1100)}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Insert(ctx, i%2, s.NextID(), e); err != nil {
			b.Fatal(err)
		}
	}
}
