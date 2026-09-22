// Owner: Claude (reviewed by Ben)
package cmek

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
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

func gcm(key *[32]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with AES-256-GCM, a fresh 96-bit nonce and AAD = tenant|msgID|dekID; refuses a handle with now >= ValidUntil.
func Seal(h Handle, now time.Time, tenant string, msgID int64, plaintext []byte) (Envelope, error) {
	if !now.Before(h.ValidUntil) {
		return Envelope{}, ErrLeaseExpired
	}
	g, err := gcm(&h.key)
	if err != nil {
		return Envelope{}, fmt.Errorf("cmek: seal: %w", err)
	}
	var env Envelope
	env.DEKID = h.DEKID
	if _, err := io.ReadFull(rand.Reader, env.Nonce[:]); err != nil {
		return Envelope{}, fmt.Errorf("cmek: nonce: %w", err)
	}
	env.Ciphertext = g.Seal(nil, env.Nonce[:], plaintext, aad(tenant, msgID, h.DEKID))
	return env, nil
}

// Open reverses Seal; a stale handle returns ErrLeaseExpired, any tag or AAD mismatch ErrPoison.
func Open(h Handle, now time.Time, tenant string, msgID int64, env Envelope) ([]byte, error) {
	if !now.Before(h.ValidUntil) {
		return nil, ErrLeaseExpired
	}
	if h.DEKID != env.DEKID {
		return nil, fmt.Errorf("%w: handle %q, envelope %q", ErrPoison, h.DEKID, env.DEKID)
	}
	g, err := gcm(&h.key)
	if err != nil {
		return nil, fmt.Errorf("cmek: open: %w", err)
	}
	pt, err := g.Open(nil, env.Nonce[:], env.Ciphertext, aad(tenant, msgID, env.DEKID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPoison, err) // tag or AAD mismatch; the worker logs and dead-letters
	}
	return pt, nil
}
