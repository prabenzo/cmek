// Owner: Claude (reviewed by Ben)
package cmek

import (
	"errors"

	"github.com/prabenzo/cmek/internal/kms"
)

// Classify maps a call result to its class, the one place the down-versus-revoked question is answered:
// nil → OK; ErrPoison → Poison; a kms.Error with AccessDenied or KeyDisabled → Deny (authoritative: the provider
// answered and said no); a kms.Error with Timeout or Unavailable, a context deadline or cancellation, and any
// unknown error → Transient (the provider did not answer, so the answer is unknown and the lease decides).
func Classify(err error) Class {
	if err == nil {
		return OK
	}
	if errors.Is(err, ErrPoison) {
		return Poison
	}
	var ke *kms.Error
	if errors.As(err, &ke) {
		switch ke.Code {
		case kms.AccessDenied, kms.KeyDisabled:
			return Deny
		}
	}
	return Transient
}
