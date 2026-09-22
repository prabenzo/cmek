// Owner: Claude
package traffic

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"
)

type sinkClock struct{}

func (sinkClock) Now() time.Time                         { return time.Now() }
func (sinkClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// TestSinkCanary: the S2 witness fires only when a payload's canary names another tenant; a payload with this
// tenant's canary or with none at all is delivered.
func TestSinkCanary(t *testing.T) {
	s := NewSink(SinkConfig{Tenants: []string{"t-0000", "t-0001"}, MinLatency: time.Millisecond, MaxLatency: 2 * time.Millisecond, Ring: 8, CanaryPrefix: "PLAINTEXT-CANARY-", Clock: sinkClock{}, Rand: rand.New(rand.NewSource(1)), Lock: &sync.Mutex{}})
	ctx := context.Background()
	rows := []struct {
		name    string
		payload string
		want    error
	}{
		{"own canary", `{"canary":"PLAINTEXT-CANARY-t-0000","x":1}`, nil},
		{"no canary", `{"x":1}`, nil},
		{"another tenant's canary", `{"canary":"PLAINTEXT-CANARY-t-0001"}`, ErrTenantMismatch},
		{"canary prefix only", `{"canary":"PLAINTEXT-CANARY-"}`, ErrTenantMismatch},
	}
	for i, r := range rows {
		_, err := s.Deliver(ctx, "t-0000", 0, int64(i), []byte(r.payload))
		if !errors.Is(err, r.want) {
			t.Errorf("%s: err = %v, want %v", r.name, err, r.want)
		}
	}
	if s.Delivered(0) != 2 || s.Mismatches() != 2 {
		t.Errorf("delivered %d, mismatches %d; want 2 and 2", s.Delivered(0), s.Mismatches())
	}
}
