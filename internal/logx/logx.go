// Owner: Claude
// Package logx is a throttled error logger: hot paths (a failing insert at 1,500/s, a poison batch) log at most one
// line per second per message, so an error is never silent and never floods.
package logx

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Throttle rate-limits Error lines per message key to one per Interval (default 1 s).
type Throttle struct {
	Log      *slog.Logger // nil disables logging
	Interval time.Duration
	last     sync.Map // msg → *atomic.Int64 (unix ms of the last line)
}

// Error logs msg with attrs unless the same msg was logged inside Interval.
func (t *Throttle) Error(msg string, attrs ...any) {
	if t.Log == nil {
		return
	}
	iv := t.Interval
	if iv <= 0 {
		iv = time.Second
	}
	v, _ := t.last.LoadOrStore(msg, new(atomic.Int64))
	stamp := v.(*atomic.Int64)
	now := time.Now().UnixMilli()
	if last := stamp.Load(); now-last >= iv.Milliseconds() && stamp.CompareAndSwap(last, now) {
		t.Log.Error(msg, attrs...)
	}
}
