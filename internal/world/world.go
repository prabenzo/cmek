// Owner: Claude (reviewed by Ben)
package world

import (
	"sync/atomic"
	"time"
)

// World owns one complete running system. In M0 it is an id, its params, a start time and a tick counter;
// M1 adds the service, the fakes and the metrics.
type World struct {
	ID      string
	P       Params
	started time.Time
	ticks   atomic.Int64
}

// HealthInfo is the /health body.
type HealthInfo struct {
	World   string  `json:"world"`
	Ticks   int64   `json:"ticks"`
	UptimeS float64 `json:"uptime_s"`
	Viewers int     `json:"viewers"`
	BurnMs  int     `json:"burn_ms"` // M0 only: the ticker's busy-burn per tick
}

// Tick increments and returns the M0 ticker counter (M1 removes it: metrics owns ticks).
func (w *World) Tick() int64 { return w.ticks.Add(1) }

// Ticks is the counter, read by /health and the stream handler.
func (w *World) Ticks() int64 { return w.ticks.Load() }

// Health builds the /health body; now and the viewer count are passed in because M0 has no clock and no hub.
func (w *World) Health(now time.Time, viewers, burnMs int) HealthInfo {
	return HealthInfo{World: w.ID, Ticks: w.Ticks(), UptimeS: now.Sub(w.started).Seconds(), Viewers: viewers, BurnMs: burnMs}
}
