// Owner: Claude (reviewed by Ben: Claim, Release)
package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/prabenzo/cmek/internal/cmek"
)

// Message is one stored row as workers see it.
type Message struct {
	ID         int64
	DEKID      string
	Nonce      [12]byte
	Ciphertext []byte
	Attempts   int
	EnqueuedAt time.Time
}

// Batch is one tenant's claimed rows.
type Batch struct {
	Tenant string
	Idx    int
	Msgs   []Message
}

// InsertStats is the M1 load instrument (/health).
type InsertStats struct {
	N, MeanUs, MaxUs, CensusMaxUs int64
}

// Clock is the store's time source.
type Clock interface{ Now() time.Time }

// Config opens one store per World.
type Config struct {
	Path              string
	Tenants           []string
	Clock             Clock
	SyncMode          string // "normal" (default) or "off"
	WALAutocheckpoint int    // pages; 0 → 4000
	Logger            *slog.Logger
}

// Store is the SQLite queue plus the in-memory ledger; every write and its ledger update share one critical section.
type Store struct {
	cfg   Config
	index map[string]int
	wdb   *sql.DB
	rdb   *sql.DB
	w     *sql.Conn
	mu    sync.Mutex

	insert, putDEK, claim, ack, release, dead, reclaim *sql.Stmt

	nextID                                              atomic.Int64
	accepted, delivered, expired, ready, claimed, deadN []atomic.Int64
	total, backlogged                                   atomic.Int64
	wake                                                chan struct{}

	insN, insSumNs, insMaxNs, censusMaxNs atomic.Int64
}

const schema = `
CREATE TABLE IF NOT EXISTS deks (
  id          TEXT    PRIMARY KEY,
  tenant_id   TEXT    NOT NULL,
  kek_id      TEXT    NOT NULL,
  kek_version INTEGER NOT NULL,
  wrapped_dek BLOB    NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY,
  tenant_id     TEXT    NOT NULL,
  dek_id        TEXT    NOT NULL,
  nonce         BLOB    NOT NULL,
  ciphertext    BLOB    NOT NULL,
  state         TEXT    NOT NULL CHECK (state IN ('ready','claimed','dead')),
  attempts      INTEGER NOT NULL DEFAULT 0,
  enqueued_at   INTEGER NOT NULL,
  claimed_until INTEGER
);
CREATE INDEX IF NOT EXISTS messages_tenant_state_id ON messages (tenant_id, state, id);
CREATE INDEX IF NOT EXISTS messages_claimed_until   ON messages (claimed_until) WHERE state = 'claimed';
`

// Open removes any stale files at Path, opens the writer (one pinned connection) and the reader pool, applies the pragmas and the schema, and prepares every statement.
func Open(cfg Config) (*Store, error) {
	if cfg.SyncMode == "" {
		cfg.SyncMode = "normal"
	}
	if cfg.WALAutocheckpoint <= 0 {
		cfg.WALAutocheckpoint = 4000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(cfg.Path + suffix)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=synchronous(%s)&_pragma=busy_timeout(5000)", cfg.Path, cfg.SyncMode)
	wdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	wdb.SetMaxOpenConns(1)
	ctx := context.Background()
	w, err := wdb.Conn(ctx)
	if err != nil {
		wdb.Close()
		return nil, err
	}
	s := &Store{cfg: cfg, index: make(map[string]int, len(cfg.Tenants)), wdb: wdb, w: w, wake: make(chan struct{}, 1)}
	for i, id := range cfg.Tenants {
		s.index[id] = i
	}
	n := len(cfg.Tenants)
	s.accepted, s.delivered, s.expired, s.ready, s.claimed, s.deadN = make([]atomic.Int64, n), make([]atomic.Int64, n), make([]atomic.Int64, n), make([]atomic.Int64, n), make([]atomic.Int64, n), make([]atomic.Int64, n)
	fail := func(err error) (*Store, error) { s.Close(); return nil, err }
	for _, p := range []string{
		fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", cfg.WALAutocheckpoint),
		"PRAGMA temp_store = MEMORY",
		"PRAGMA cache_size = -65536",
	} {
		if _, err := w.ExecContext(ctx, p); err != nil {
			return fail(fmt.Errorf("pragma %q: %w", p, err))
		}
	}
	if _, err := w.ExecContext(ctx, schema); err != nil {
		return fail(fmt.Errorf("schema: %w", err))
	}
	var jm, sy, ac string
	_ = w.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm)
	_ = w.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sy)
	_ = w.QueryRowContext(ctx, "PRAGMA wal_autocheckpoint").Scan(&ac)
	cfg.Logger.Info("sqlite open", "path", cfg.Path, "journal_mode", jm, "synchronous", sy, "wal_autocheckpoint", ac)
	prep := func(dst **sql.Stmt, q string) error {
		st, err := w.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("prepare %q: %w", q, err)
		}
		*dst = st
		return nil
	}
	for _, x := range []struct {
		dst **sql.Stmt
		q   string
	}{
		{&s.insert, `INSERT INTO messages (id, tenant_id, dek_id, nonce, ciphertext, state, attempts, enqueued_at) VALUES (?1,?2,?3,?4,?5,'ready',0,?6)`},
		{&s.putDEK, `INSERT INTO deks (id, tenant_id, kek_id, kek_version, wrapped_dek, created_at) VALUES (?1,?2,?3,?4,?5,?6)`},
		{&s.claim, `UPDATE messages SET state='claimed', claimed_until=?1 WHERE id IN (SELECT id FROM messages WHERE tenant_id=?2 AND state='ready' ORDER BY id LIMIT ?3) RETURNING id, dek_id, nonce, ciphertext, attempts, enqueued_at`},
		{&s.ack, `DELETE FROM messages WHERE state='claimed' AND id IN (SELECT value FROM json_each(?1))`},
		{&s.release, `UPDATE messages SET state='ready', claimed_until=NULL WHERE state='claimed' AND id IN (SELECT value FROM json_each(?1))`},
		{&s.dead, `UPDATE messages SET state='dead', claimed_until=NULL, attempts=attempts+1 WHERE id=?1 AND state='claimed'`},
		{&s.reclaim, `UPDATE messages SET state='ready', claimed_until=NULL WHERE state='claimed' AND claimed_until < ?1 RETURNING tenant_id`},
	} {
		if err := prep(x.dst, x.q); err != nil {
			return fail(err)
		}
	}
	rdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fail(err)
	}
	rdb.SetMaxOpenConns(2)
	s.rdb = rdb
	return s, nil
}

// Close closes every connection and removes the three database files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range []*sql.Stmt{s.insert, s.putDEK, s.claim, s.ack, s.release, s.dead, s.reclaim} {
		if st != nil {
			st.Close()
		}
	}
	var err error
	if s.w != nil {
		err = errors.Join(err, s.w.Close())
	}
	if s.wdb != nil {
		err = errors.Join(err, s.wdb.Close())
	}
	if s.rdb != nil {
		err = errors.Join(err, s.rdb.Close())
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(s.cfg.Path + suffix)
	}
	return err
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// NextID allocates a monotone message id without taking the writer lock.
func (s *Store) NextID() int64 { return s.nextID.Add(1) }

func (s *Store) backlogOf(i int) int64 { return s.ready[i].Load() + s.claimed[i].Load() }

func (s *Store) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Insert writes one sealed row and updates the ledger in one critical section.
func (s *Store) Insert(ctx context.Context, idx int, id int64, env cmek.Envelope) error {
	now := s.cfg.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	start := s.cfg.Clock.Now()
	_, err := s.insert.ExecContext(ctx, id, s.cfg.Tenants[idx], env.DEKID, env.Nonce[:], env.Ciphertext, ms(now))
	d := s.cfg.Clock.Now().Sub(start).Nanoseconds()
	s.insN.Add(1)
	s.insSumNs.Add(d)
	for {
		m := s.insMaxNs.Load()
		if d <= m || s.insMaxNs.CompareAndSwap(m, d) {
			break
		}
	}
	if err != nil {
		return err
	}
	if s.backlogOf(idx) == 0 {
		s.backlogged.Add(1)
	}
	s.accepted[idx].Add(1)
	s.ready[idx].Add(1)
	s.total.Add(1)
	s.signal()
	return nil
}

// PutDEK persists a wrapped DEK (wrapped bytes only).
func (s *Store) PutDEK(ctx context.Context, d cmek.WrappedDEK) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.putDEK.ExecContext(ctx, d.ID, d.Tenant, d.KEKID, d.KEKVersion, d.Wrapped, ms(d.CreatedAt))
	return err
}

// Claim marks up to n ready rows of one tenant claimed until `until` and returns them; the RETURNING cursor is always drained to completion.
func (s *Store) Claim(ctx context.Context, idx, n int, until time.Time) (Batch, error) {
	b := Batch{Tenant: s.cfg.Tenants[idx], Idx: idx}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.claim.QueryContext(context.Background(), ms(until), b.Tenant, n)
	if err != nil {
		return b, err
	}
	var scanErr error
	drained := 0
	for rows.Next() {
		drained++
		var m Message
		var nonce []byte
		var enq int64
		if err := rows.Scan(&m.ID, &m.DEKID, &nonce, &m.Ciphertext, &m.Attempts, &enq); err != nil {
			scanErr = err
			continue
		}
		copy(m.Nonce[:], nonce)
		m.EnqueuedAt = time.UnixMilli(enq)
		b.Msgs = append(b.Msgs, m)
	}
	rows.Close()
	changed := int64(drained)
	if scanErr != nil {
		_ = s.w.QueryRowContext(context.Background(), "SELECT changes()").Scan(&changed)
	}
	s.ready[idx].Add(-changed)
	s.claimed[idx].Add(changed)
	if scanErr != nil {
		return b, scanErr
	}
	return b, nil
}

func jsonIDs(ids []int64) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

// Ack deletes delivered rows; the ledger moves by exactly the rows affected.
func (s *Store) Ack(ctx context.Context, idx int, ids []int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.ack.ExecContext(ctx, jsonIDs(ids))
	if err != nil {
		return 0, err
	}
	ra, _ := res.RowsAffected()
	s.delivered[idx].Add(ra)
	s.claimed[idx].Add(-ra)
	s.total.Add(-ra)
	if ra > 0 && s.backlogOf(idx) == 0 {
		s.backlogged.Add(-1)
	}
	return int(ra), nil
}

// Release returns claimed rows to ready with their attempts untouched.
func (s *Store) Release(ctx context.Context, idx int, ids []int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.release.ExecContext(ctx, jsonIDs(ids))
	if err != nil {
		return 0, err
	}
	ra, _ := res.RowsAffected()
	s.claimed[idx].Add(-ra)
	s.ready[idx].Add(ra)
	s.signal()
	return int(ra), nil
}

// Dead dead-letters one claimed row (poison, or an S2 mismatch at the sink).
func (s *Store) Dead(ctx context.Context, idx int, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.dead.ExecContext(ctx, id)
	if err != nil {
		return err
	}
	ra, _ := res.RowsAffected()
	s.claimed[idx].Add(-ra)
	s.deadN[idx].Add(ra)
	s.total.Add(-ra)
	if ra > 0 && s.backlogOf(idx) == 0 {
		s.backlogged.Add(-1)
	}
	return nil
}

// Reclaim returns timed-out claims to ready (at-least-once delivery); the sweep calls it every ReclaimInterval.
func (s *Store) Reclaim(ctx context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.reclaim.QueryContext(context.Background(), ms(now))
	if err != nil {
		return 0, err
	}
	n := 0
	for rows.Next() {
		var tenant string
		if err := rows.Scan(&tenant); err != nil {
			continue
		}
		if i, ok := s.index[tenant]; ok {
			s.claimed[i].Add(-1)
			s.ready[i].Add(1)
			n++
		}
	}
	rows.Close()
	if n > 0 {
		s.signal()
	}
	return n, nil
}

// Stats returns the insert timing instrument.
func (s *Store) Stats() InsertStats {
	n := s.insN.Load()
	st := InsertStats{N: n, MaxUs: s.insMaxNs.Load() / 1000, CensusMaxUs: s.censusMaxNs.Load() / 1000}
	if n > 0 {
		st.MeanUs = s.insSumNs.Load() / n / 1000
	}
	return st
}

// Backlog, Ready, Total and Backlogged are lock-free ledger reads used by admission, the scheduler and metrics.
func (s *Store) Backlog(idx int) int { return int(s.backlogOf(idx)) }
func (s *Store) Ready(idx int) int   { return int(s.ready[idx].Load()) }
func (s *Store) Total() int          { return int(s.total.Load()) }
func (s *Store) Backlogged() int     { return int(s.backlogged.Load()) }

// Wake fires (cap 1, non-blocking) after an insert, release or reclaim.
func (s *Store) Wake() <-chan struct{} { return s.wake }
