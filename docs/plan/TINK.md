# Tink adoption — replace the hand-rolled AES-GCM with tink-go (branch `claude/tink-go`, merges into the M4 pull request)

> References: `DESIGN-REFERENCE.md` is the detailed design; "ARCH › section" is shorthand for it. This plan amends it where marked **amends ARCH**. Status at the bottom.

## Overview

**What this change accomplishes.**

- The two places that roll their own cryptography are replaced by [tink-go](https://github.com/tink-crypto/tink-go) v2.8.0 primitives: the message envelope in `internal/cmek/envelope.go` (AES-256-GCM with a hand-managed nonce and AAD) and the KEK wrapping in `internal/kms/fake.go` (AES-256-GCM under a per-KEK key). After the change, no file in the repo calls `crypto/aes` or `crypto/cipher`.
- `internal/cmek` keeps exactly what it is good at: the lease state machine, the DEK cache, singleflight fetches, probes and warms, bulkheads, purge on revoke. A cached DEK becomes a Tink keyset (`*keyset.Handle`) plus its `tink.AEAD` primitive instead of a `[32]byte`; the wrapped form persisted in the `deks` table becomes the serialized keyset encrypted by the tenant's KMS key. Sealing and opening a message is `prim.Encrypt(plaintext, aad)` / `prim.Decrypt(ciphertext, aad)` with the same AAD as today (`tenant | msgID | dekID`).
- The KMS boundary becomes Tink's own model of a KMS: a *remote AEAD per KEK* that encrypts and decrypts small blobs (the DEK keyset). The fake providers implement it with a real Tink AES-256-GCM key per KEK behind the same fault, latency, fast-fail and revoke gate as today, and the ground-truth log is unchanged. A real adapter is then whatever the `tink-go-gcpkms` or `tink-go-awskms` extension module already provides, wrapped in six lines, which is a better answer to the spec's "the interface a real adapter would implement" than our own `GenerateDataKey`/`Unwrap` pair.
- Every behaviour the scenarios and the checker depend on is preserved: the lease timings, the four states, the audit ops (`generate`, `unwrap`, `state`), the timeline lines, `ErrPoison` on a tag or AAD mismatch, the 403/503 mapping, S1 (no plaintext at rest), S2 (AAD binds tenant, message and DEK), S3 (no decrypt after the lease bound: the purge drops the primitive). The wire and storage formats change (Tink ciphertext carries its own IV), and since every World creates its database fresh, no migration exists.
- What does **not** change: the queue, scheduler, workers (one call site), admission, scenarios, metrics, the page, the parameters. Nothing in the UI knows which library sealed a row.

**What is deliberately not done.**

- Tink's `KMSEnvelopeAEAD` (one KMS call per message, a fresh DEK each time) is not used on the message path: it is the opposite of the lease-and-cache thesis and would make L2 ("KMS calls stay flat under a 100× surge") false by construction. Tink's `keyset.ReadWithContext` / `WriteWithContext` helpers are not used for the KMS round trip either, for a concrete reason given under Decisions: they format the remote error with `%v` (`keyset/handle.go`, `decryptWithContext`), which erases the `*kms.Error` code our classifier reads, so a revoked key would classify as Transient and never reach REVOKED. The KMS call is made by our fetcher directly on the remote AEAD and the keyset is parsed with `insecurecleartextkeyset`, Tink's sanctioned API for key material a process legitimately holds in memory.
- No KEK rotation, no keyset with several keys, no Tink key-id prefix on message ciphertext (Decisions). The `kek_version` column stays at 1.

**Size and where it lands.** ≈ 260 lines changed across six Go files and one test file, ≈ 40 lines net fewer in `internal/cmek` (the bound the plans track). One dependency and its two transitive modules (`google.golang.org/protobuf`, `golang.org/x/crypto`). Branch `claude/tink-go` cut from the M4 head (`4f2a53b`), reviewed as its own pull request into `claude/epic-rubin-kgewov`, so the M4 pull request carries it. Nothing here waits on M5.

## Files

| Path | Change | Purpose | ~Lines |
| --- | --- | --- | --- |
| `go.mod`, `go.sum` | Mod | `github.com/tink-crypto/tink-go/v2 v2.8.0` (requires Go ≥ 1.25; we are on 1.26) | +3 |
| `internal/kms/kms.go` | Mod | `KMS` interface becomes `KEK(kekID string) (tink.AEADWithContext, error)`; `DataKey` deleted; `Code`, `Error`, `KeyEvent`, `Clock` unchanged | −12 / +8 |
| `internal/kms/fake.go` | Mod | per-KEK key is a Tink AES-256-GCM keyset (`keyset.NewHandle(aead.AES256GCMKeyTemplate())` → `aead.New`); `GenerateDataKey`/`Unwrap` replaced by one `gate` type implementing `tink.AEADWithContext` that runs the existing `begin` (fault resolve → truth → fast-fail / error rate / latency raced with ctx → enabled read after latency) and then delegates to the KEK primitive with AAD = kekID; `Revoke`/`Restore`/`SetFault`/`Truth` untouched | −45 / +40 |
| `internal/cmek/envelope.go` | Mod | `Seal`/`Open` on `Handle.prim` (`tink.AEAD`): lease check, `Encrypt`/`Decrypt` with the same `aad()`, `ErrPoison` on any decrypt error; `Envelope` loses `Nonce`; no `crypto/*` imports | 66 → ≈ 34 |
| `internal/cmek/types.go` | Mod | `Handle{DEKID, ValidUntil, prim tink.AEAD}`; `Zero()` drops the reference (Decisions); `Envelope{DEKID, Ciphertext}`; `WrappedDEK` unchanged; `Config.Keys kms.KMS` unchanged | ±10 |
| `internal/cmek/lease.go` | Mod | `dek.key *[32]byte` → `dek.prim tink.AEAD` (nil while cold); `purge()` sets `prim = nil` | ±6 |
| `internal/cmek/fetcher.go` | Mod | `result.key` → `result.prim`; `generate`: `keyset.NewHandle(aead.AES256GCMNoPrefixKeyTemplate())` → serialize with `insecurecleartextkeyset.Write` → **the KMS call** `kek.EncryptWithContext(ctx, serialized, aad)` → `PutDEK(wrapped)` → `aead.New(handle)`; `unwrap` (renew / probe / warm): **the KMS call** `kek.DecryptWithContext(ctx, wrapped, aad)` → `insecurecleartextkeyset.Read` → `aead.New`; `call` (tenant cap, provider semaphore, `KMSTimeout` ctx, `sentAt`) unchanged, it wraps a `func(ctx) ([]byte, error)` | ±40 |
| `internal/cmek/manager.go` | Mod | `handle()` and `DecryptKey` copy `d.prim` instead of `*d.key`; `Tick`'s age purge nils the primitive; nothing else | ±8 |
| `internal/queue/store.go` | Mod | `messages.nonce` column dropped; `Insert`/`Claim` bind `ciphertext` only; `Message` loses `Nonce` | −8 |
| `internal/queue/workers.go` | Mod | the one `cmek.Open` call site builds `Envelope{DEKID, Ciphertext}` | ±1 |
| `internal/world/ingest.go` | Mod | `h.Zero()` stays (a no-op that drops the primitive; Decisions) | 0 |
| `internal/cmek/cmek_test.go` | Mod | rig: `latentKMS` wraps `KEK()`; `testHandle` builds a Tink primitive; `TestEnvelope` keeps its rows (round trip, wrong tenant / msgID / dekID → `ErrPoison`, stale handle); new rows: tampered ciphertext → `ErrPoison`; **interop**: a `tink.AEAD` built independently from the same keyset decrypts our `Envelope.Ciphertext` with our AAD (proves the ciphertext is plain Tink AES-GCM, IV embedded); **wrapped form**: the `deks` blob decrypts only under its own KEK (another KEK → `AccessDenied`) and parses as a one-key AES-256-GCM keyset | ±60 |
| `internal/kms/fake_test.go` | New | the gate: fast-fail → `Unavailable`, error rate 1.0 → `Unavailable`, latency past the ctx deadline → `Timeout`, disabled KEK → `KeyDisabled` **after** the latency, truth records the call and the flip in order; unknown KEK → `AccessDenied` | 60 |
| `docs/plan/DESIGN-REFERENCE.md`, `CONTEXT.md`, `ARCH-NOTES.md`, `docs/SPEC.md` | Mod | amendments row "Tink"; the cmek import list and the CI regex; the `Seal`/`Open` signatures; the fake's description; the `messages` schema; SPEC › Libraries row and the "Real AES-256-GCM wrapping" sentence gain "via tink-go"; CONTEXT's allowed-library line | ≈ 12 lines |

Budget: `internal/cmek` ≈ 730 → ≈ 690 code lines (the M2 note's ≤ 600 bound is not reached, but the direction is right); `internal/kms` ≈ 300 → ≈ 300.

## Interfaces and types (after)

```go
package kms

// KMS is what a real provider adapter implements: one remote AEAD per KEK. The call is offline (no network); the
// AEAD's methods are the KMS round trips. A real adapter wraps the tink.AEAD a tink-go-gcpkms / tink-go-awskms
// registry.KMSClient returns for the key URI, ignoring ctx or threading it through its own HTTP client.
type KMS interface {
	KEK(kekID string) (tink.AEADWithContext, error) // unknown kekID → *Error{Code: AccessDenied}
}
```

```go
package cmek

// Handle is one usable DEK primitive until ValidUntil; Seal never blocks and never touches a lock.
type Handle struct {
	DEKID      string
	ValidUntil time.Time
	prim       tink.AEAD
}
func (h *Handle) Zero() { h.prim = nil } // drops the reference; Tink does not zeroize key bytes (Decisions)

// Envelope is what the queue stores: the DEK id and Tink's ciphertext (IV || AES-256-GCM output incl. tag).
type Envelope struct {
	DEKID      string
	Ciphertext []byte
}

// Seal encrypts plaintext under the handle's DEK with AAD = tenant|msgID|dekID; refuses a handle with now >= ValidUntil.
func Seal(h Handle, now time.Time, tenant string, msgID int64, plaintext []byte) (Envelope, error)
// Open reverses Seal; a stale handle returns ErrLeaseExpired, a DEK id mismatch or any decrypt failure ErrPoison.
func Open(h Handle, now time.Time, tenant string, msgID int64, env Envelope) ([]byte, error)
```

Fetcher internals (unexported; the only new code paths):

```go
// newDEK generates a fresh AES-256-GCM keyset locally (crypto/rand inside Tink) and wraps it with one KMS call.
func (m *Manager) newDEK(ctx context.Context, kek tink.AEADWithContext, kekID string) (wrapped []byte, prim tink.AEAD, err error)
//   h, _ := keyset.NewHandle(aead.AES256GCMNoPrefixKeyTemplate())
//   var buf bytes.Buffer; insecurecleartextkeyset.Write(h, keyset.NewBinaryWriter(&buf))
//   wrapped, err = kek.EncryptWithContext(ctx, buf.Bytes(), []byte(kekID))   // the KMS call; err is the adapter's *kms.Error, unwrapped
//   prim, _ = aead.New(h)

// openDEK recovers the primitive from the wrapped keyset with one KMS call (renewal, probe, warm).
func (m *Manager) openDEK(ctx context.Context, kek tink.AEADWithContext, kekID string, wrapped []byte) (tink.AEAD, error)
//   clear, err := kek.DecryptWithContext(ctx, wrapped, []byte(kekID))         // the KMS call
//   h, _ := insecurecleartextkeyset.Read(keyset.NewBinaryReader(bytes.NewReader(clear)))
//   return aead.New(h)
```

The audit op names stay `generate` (one wrap call after a local generation) and `unwrap`; `Classify` is untouched because the error the fetcher sees is the adapter's own `*kms.Error`.

`internal/cmek` imports after the change: today's list minus `crypto/aes`, `crypto/cipher`, `crypto/rand`, `encoding/binary` stays (`aad`), plus `bytes`, `github.com/tink-crypto/tink-go/v2/aead`, `.../keyset`, `.../insecurecleartextkeyset`, `.../tink`. CI import check (**amends ARCH** › internal/cmek): `go list -deps ./internal/cmek | grep -vE '^(golang.org/x/(sync|crypto)|google.golang.org/protobuf|github.com/tink-crypto/tink-go/v2|github.com/prabenzo/cmek/internal/(cmek|kms|logx)$|[a-z0-9./]+$)'` produces no output.

## Order of work (one lane, Claude; Ben reviews the pull request)

1. **T+0–4 spike.** `go get` the module; a throwaway test confirms three facts before any refactor: (a) `errors.As` does **not** see a `*kms.Error` through `keyset.ReadWithContext` (the reason for the direct-AEAD path), (b) a `NoPrefix` AES-256-GCM primitive's ciphertext is `12-byte IV || ct || 16-byte tag` and decrypts with the stdlib for the interop test, (c) `insecurecleartextkeyset.Write`/`Read` round-trips a one-key keyset to ≈ 80–110 bytes. The test is deleted at T+4; its findings go in the TIMELOG note.
2. **T+4–12 kms.** Interface, the `gate`, `fake_test.go`; `go test -race ./internal/kms`.
3. **T+12–24 cmek.** `types.go`, `lease.go`, `envelope.go`, `fetcher.go` (`newDEK`/`openDEK`, `result.prim`), `manager.go`; the test rig; the new `TestEnvelope` rows; `go test -race ./internal/cmek` (the 11-row walk, the extras and the concurrent cold callers must pass unchanged: they assert states, audits and call counts, none of which move).
4. **T+24–30 queue and world.** Drop `nonce`, fix `Insert`/`Claim`/`Message`, the worker's `Open` call; `go build ./... && go vet ./... && go test -race ./...`; the no-globals grep and the new import regex.
5. **T+30–36 acceptance.** Local run on `/dev/shm` at 300/s from the stream (the M4 script's blip and revocation legs): blip recovers ≤ 12 s, revocation purple ≤ 30 s with exactly one `REVOKED, N DEKs purged` line and `403 key_revoked`, Restore → green ≤ 5 s, `kms_ps.deny` non-zero during the revoke (proves the code survives Tink), `ingest_ps.internal` 0, 0 server errors. A `TestNoPlaintextAtRest` row in `store_test.go` inserts a sealed canary and asserts the stored `ciphertext` blob does not contain it (S1's mechanism, cheap to pin here).
6. **T+36–40 docs.** The amendment rows, the SPEC library line, the TIMELOG row for this branch, `README.md` › decisions table row (approval record).

Estimated ≈ 40 working minutes at the measured pace. Pushed at T+12 (kms), T+24 (cmek), T+40 (final).

## Tests

| Test | File | Asserts |
| --- | --- | --- |
| `TestEnvelope` (extended) | cmek_test.go | round trip; wrong tenant / msgID / dekID → `ErrPoison`; flipped ciphertext byte → `ErrPoison`; stale handle → `ErrLeaseExpired` from both; **interop**: `aead.New(sameHandle).Decrypt(env.Ciphertext, aad)` yields the plaintext |
| `TestWrappedDEK` (new) | cmek_test.go | after one `EncryptKey`, `mapStore` holds one `WrappedDEK` whose bytes decrypt under the tenant's KEK gate into a one-key AES-256-GCM keyset and fail under another tenant's KEK with `AccessDenied`; the walk's revoke row still sees `KeyDisabled` classified `Deny` |
| `TestGate` (new) | kms/fake_test.go | fast-fail → `Unavailable` before any latency; error rate 1 → `Unavailable`; latency 2 s vs 500 ms ctx → `Timeout`; disabled KEK → `KeyDisabled` after the latency; truth order call → flip → deny; unknown KEK → `AccessDenied` |
| `TestNoPlaintextAtRest` (new) | queue/store_test.go | a sealed canary payload's stored blob has no canary substring and no plaintext bytes |
| existing walk, extras, concurrency, world, scheduler, gate, hist | unchanged | green under `-race` with no edits beyond the rig |

## Decisions for Ben

**Status: plan drafted 2026-09-22 on branch `claude/tink-go`; awaiting Ben's approval before any code (standing process). Each bullet's first option is the recommendation.**

- **KMS round trip: direct AEAD calls + `insecurecleartextkeyset` (recommended) vs Tink's `keyset.ReadWithContext`/`WriteWithContext`.** The helpers wrap the remote error with `%v`, not `%w` (`keyset/handle.go`: `fmt.Errorf("keyset.Handle: decryption failed: %v", err)`), so `Classify` would see a plain string: `KeyDisabled` and `AccessDenied` would both become Transient, the REVOKED state would never be entered and S3's bound would hold only by lease expiry. The direct path keeps the adapter's `*kms.Error` first-hand and uses Tink for every byte of cryptography. Alternative: the helpers plus string matching on the error text, which is brittle and is not recommended.
- **Message ciphertext template: `AES256GCMNoPrefixKeyTemplate` (recommended) vs `AES256GCMKeyTemplate` (5-byte Tink key-id prefix).** One key per keyset and our own `dek_id` column already route decryption, so the prefix buys nothing and costs 5 bytes per row and a non-standard layout. The KEK keysets in the fake use the prefixed template (Tink's default for a keyset that could rotate).
- **Drop the `messages.nonce` column (recommended) vs keep it empty.** Tink embeds the IV in the ciphertext; the database is created fresh per World, so there is nothing to migrate. Keeping the column would leave a `NOT NULL` blob written empty on every insert.
- **`Handle.Zero()` stays as a reference drop (recommended) vs deleting it.** Tink holds key material in ordinary Go memory and offers no zeroization, so the explicit wipe the design promised (`Zero wipes the key bytes`) no longer exists in any form; the honest statement for the README rationale is "plaintext DEKs are dropped on purge and collected by the GC; they are not zeroized". The call site in `ingest.go` keeps compiling and the intent stays documented. Alternative: delete the method and the call. Either way `DESIGN-REFERENCE` › Concurrency ("Seal runs on a Handle copy outside every lock") still holds: the copy is an interface value.
- **Fake providers keep their own gate around a Tink AEAD (recommended) vs Tink's `testing/fakekms`.** `fakekms` embeds the key in the URI and has no faults, latency, revoke or ground truth; every behaviour the scenarios need lives in our gate anyway, so wrapping `fakekms` would add a layer and remove nothing.
- **Library list amendment.** The spec's "Libraries" row rationale ("little to review beyond our own code") changes character: we trade ≈ 130 lines of our own AES-GCM for a vetted library plus `protobuf` and `x/crypto` as transitive modules (three modules, no CGO, pure Go). Recommended: amend SPEC › Libraries and CONTEXT › allowed libraries to add `tink-go`, and say so in the README rationale as a deliberate choice. This is the one decision that is a spec change rather than a design one.
- **Scope: this branch only touches crypto.** Recommended: no KEK rotation, no keyset-level DEK rotation (our `DEKMaxMessages`/`DEKMaxAge` rotation stays a new keyset per DEK id), no `KMSEnvelopeAEAD`. Alternative: model DEK rotation as one Tink keyset per tenant with rotating primary keys, which would move the DEK cache into Tink's `keyset.Manager` and is a larger refactor with no scenario visible from it.

## Risks

- **Error classification through Tink** is the one correctness risk and is designed out (direct AEAD calls); the spike at T+0 and `TestWrappedDEK`'s revoke row pin it.
- **Throughput.** Tink's AES-GCM is the stdlib's underneath; the primitive is built once per DEK at fetch time and cached in `dek.prim`, never per message. Expect the delivered plateau (≈ 620/s, synthetic) and the ingest cost (≈ 2 µs) unchanged; the M4 acceptance leg at T+30 reads both.
- **Dependency and build.** Three new modules, pure Go, no CGO, `go 1.25` minimum (we pin 1.26); the Dockerfile's `golang:1.26` builder needs no change. `go mod tidy` is run once and the sums committed.
- **Storage size.** The wrapped DEK grows from 60 bytes to ≈ 100–130 (serialized keyset + IV + tag); at ≤ 3 DEKs per tenant this is ≈ 400 KB per World. Message rows shrink by the 12-byte nonce column and grow by the 12-byte embedded IV: net zero.
- **Review surface.** The diff is mechanical outside `fetcher.go`'s two new helpers and the fake's gate; those three functions are where Ben's review time goes.

## Status

**Awaiting approval.** No code on this branch beyond this document. On approval the build runs on `claude/tink-go`, Ben opens its pull request into `claude/epic-rubin-kgewov`, and the M4 pull request carries the result.
