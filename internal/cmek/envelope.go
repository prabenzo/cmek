// Owner: Claude (reviewed by Ben)
package cmek

import (
	"encoding/binary"
	"fmt"
	"time"
)

// aad binds a ciphertext to its tenant, message id and DEK id: tenant | msgID (8 bytes big-endian) | dekID.
func aad(tenant string, msgID int64, dekID string) []byte {
	b := make([]byte, 0, len(tenant)+8+len(dekID))
	b = append(b, tenant...)
	b = binary.BigEndian.AppendUint64(b, uint64(msgID))
	b = append(b, dekID...)
	return b
}

// Seal encrypts plaintext under the handle's DEK (Tink AES-256-GCM: the IV is drawn inside and embedded in the
// ciphertext) with AAD = tenant|msgID|dekID; refuses a handle with now >= ValidUntil or one already zeroed.
func Seal(h Handle, now time.Time, tenant string, msgID int64, plaintext []byte) (Envelope, error) {
	if !now.Before(h.ValidUntil) {
		return Envelope{}, ErrLeaseExpired
	}
	if h.prim == nil {
		return Envelope{}, fmt.Errorf("cmek: seal: %w", ErrDEKCold)
	}
	ct, err := h.prim.Encrypt(plaintext, aad(tenant, msgID, h.DEKID))
	if err != nil {
		return Envelope{}, fmt.Errorf("cmek: seal: %w", err)
	}
	return Envelope{DEKID: h.DEKID, Ciphertext: ct}, nil
}

// Open reverses Seal; a stale handle returns ErrLeaseExpired, a DEK id mismatch or any decrypt failure ErrPoison.
func Open(h Handle, now time.Time, tenant string, msgID int64, env Envelope) ([]byte, error) {
	if !now.Before(h.ValidUntil) {
		return nil, ErrLeaseExpired
	}
	if h.DEKID != env.DEKID {
		return nil, fmt.Errorf("%w: handle %q, envelope %q", ErrPoison, h.DEKID, env.DEKID)
	}
	if h.prim == nil {
		return nil, fmt.Errorf("cmek: open: %w", ErrDEKCold)
	}
	pt, err := h.prim.Decrypt(env.Ciphertext, aad(tenant, msgID, env.DEKID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPoison, err) // tag or AAD mismatch; the worker logs and dead-letters
	}
	return pt, nil
}
