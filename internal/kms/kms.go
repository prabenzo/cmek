// Owner: Claude (reviewed by Ben)
package kms

import (
	"context"
	"fmt"
	"time"
)

// DataKey is a freshly generated DEK: plaintext for memory, Wrapped for storage.
type DataKey struct {
	Plaintext  [32]byte
	Wrapped    []byte
	KEKVersion int
}

// KMS is the interface a real provider adapter implements; only cmek calls it.
type KMS interface {
	GenerateDataKey(ctx context.Context, kekID string) (DataKey, error)
	Unwrap(ctx context.Context, kekID string, kekVersion int, wrapped []byte) ([32]byte, error)
}

// Code is the provider-neutral error category a real adapter maps its SDK errors to.
type Code uint8

const (
	Timeout Code = iota + 1
	Unavailable
	AccessDenied
	KeyDisabled
)

// String names the code.
func (c Code) String() string {
	switch c {
	case Timeout:
		return "Timeout"
	case Unavailable:
		return "Unavailable"
	case AccessDenied:
		return "AccessDenied"
	case KeyDisabled:
		return "KeyDisabled"
	}
	return "Unknown"
}

// Error is every failure a KMS returns; Classify reads Code only.
type Error struct {
	Code          Code
	Provider, Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("kms %s: %s: %s", e.Provider, e.Code, e.Msg) }

// KeyEvent is a ground-truth key state change; At is read inside the same critical section that flips the key.
type KeyEvent struct {
	KEKID   string
	Idx     int
	At      time.Time
	Enabled bool
}

// Clock is the fake's time source.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}
