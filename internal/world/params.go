// Owner: Claude
package world

import "time"

// Params holds every time constant and load setting; M0 carries three fields, later milestones add the rest.
type Params struct {
	Tenants          int           // 1000
	SnapshotInterval time.Duration // 500ms (2 Hz)
	IdleRebuild      time.Duration // 10s: no viewers longer than this → next connection builds a fresh World (M5)
}

// Demo returns the spec's demo values.
func Demo() Params {
	return Params{Tenants: 1000, SnapshotInterval: 500 * time.Millisecond, IdleRebuild: 10 * time.Second}
}
