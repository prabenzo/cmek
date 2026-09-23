// Owner: Claude (reviewed by Ben)
package kms

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tink-crypto/tink-go/v2/tink"
)

// KMS is what a real provider adapter implements: one remote AEAD per KEK, Tink's own model of a key management
// service. KEK is offline (a lookup, no network); the AEAD's EncryptWithContext and DecryptWithContext are the KMS
// round trips that wrap and unwrap a DEK keyset. A real adapter wraps the tink.AEAD a tink-go-gcpkms or
// tink-go-awskms client returns for the key URI. An unknown kekID answers *Error{Code: AccessDenied}. Only cmek
// calls it.
type KMS interface {
	KEK(kekID string) (tink.AEADWithContext, error)
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

// Error is every failure a KMS returns; Classify reads Code only. Its text is a frame CodeFromText can read back.
type Error struct {
	Code          Code
	Provider, Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("kms %s: %s: %s", e.Provider, e.Code, e.Msg) }

// CodeFromText recovers the Code of an *Error whose type a caller erased by formatting it with %v. Tink's keyset
// helpers do exactly that around the remote AEAD's error ("keyset.Handle: decryption failed: kms gcp: KeyDisabled:
// kek-t-0754 is disabled"), so the cmek classifier reads the code back from the text. It reads the code out of the
// first "kms <provider>: <Code>: " frame in the text (the outermost one is the adapter's own; a later one is
// upstream text the adapter echoed in Msg), or maps a context deadline flattened to its text to Timeout; ok is
// false for any other text. TestCodeThroughTink drives the real helpers against the fake so a Tink upgrade that
// stops including the cause fails the unit tests, not the revocation scenario.
func CodeFromText(err error) (code Code, ok bool) {
	if err == nil {
		return 0, false
	}
	s := err.Error()
	if _, c, ok := frame(s); ok {
		return c, true
	}
	if strings.Contains(s, context.DeadlineExceeded.Error()) {
		return Timeout, true
	}
	return 0, false
}

// Cause trims an error to what a person needs to read: the text from the first "kms <provider>: <Code>: " frame
// onward, or from a flattened context deadline onward, else the whole text. The audit Detail the timeline and the
// tenant panel show goes through here, so Tink's "keyset.Handle: decryption failed: " prefix stays out of the UI.
func Cause(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i, _, ok := frame(s); ok {
		return s[i:]
	}
	if i := strings.Index(s, context.DeadlineExceeded.Error()); i >= 0 {
		return s[i:]
	}
	return s
}

// frame finds the first "kms <provider>: <Code>: " frame in s whose Code is one of ours and returns its start.
func frame(s string) (start int, code Code, ok bool) {
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], "kms ")
		if j < 0 {
			return 0, 0, false
		}
		at := i + j
		rest := s[at+len("kms "):]
		if k := strings.Index(rest, ": "); k >= 0 {
			rest = rest[k+2:]
			for _, c := range []Code{Timeout, Unavailable, AccessDenied, KeyDisabled} {
				if strings.HasPrefix(rest, c.String()+": ") {
					return at, c, true
				}
			}
		}
		i = at + len("kms ")
	}
	return 0, 0, false
}

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
