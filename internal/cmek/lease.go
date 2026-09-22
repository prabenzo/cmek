// Owner: Claude (reviewed by Ben)
package cmek

import "time"

// dek is one DEK: wrapped bytes forever, plaintext only while hot. key == nil means cold.
type dek struct {
	id         string
	wrapped    []byte
	kekVersion int
	key        *[32]byte
	msgs       int
	createdAt  time.Time
}

// tenant is everything m.mu protects for one tenant (M1 shape; M2 grows it in place).
type tenant struct {
	idx    int
	spec   TenantSpec
	seq    int
	active *dek
	deks   map[string]*dek
}
