// Owner: Claude
package kms

import "time"

// Mode is a fault mode.
type Mode uint8

const (
	ModeOK Mode = iota
	ModeFastFail
)

// Fault is one fault setting; zero fields mean "inherit from the provider setting".
type Fault struct {
	Mode      Mode
	P50, P99  time.Duration
	ErrorRate float64
}

// IsZero reports whether the fault changes nothing (SetFault with a zero Fault clears the scope).
func (f Fault) IsZero() bool { return f == Fault{} }

// Scope names one provider or one KEK.
type Scope struct{ Provider, KEKID string }

// resolve merges a KEK-scoped fault over a provider-scoped one field by field where the KEK fault sets a value.
func resolve(prov, kek Fault) Fault {
	out := prov
	if kek.Mode != ModeOK {
		out.Mode = kek.Mode
	}
	if kek.P50 > 0 {
		out.P50 = kek.P50
	}
	if kek.P99 > 0 {
		out.P99 = kek.P99
	}
	if kek.ErrorRate > 0 {
		out.ErrorRate = kek.ErrorRate
	}
	return out
}
