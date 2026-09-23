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

- Tink's `KMSEnvelopeAEAD` (one KMS call per message, a fresh DEK each time) is not used on the message path: it is the opposite of the lease-and-cache thesis and would make L2 ("KMS calls stay flat under a 100× surge") false by construction.
- `insecurecleartextkeyset` is not imported anywhere. The KMS round trip uses Tink's own helpers, `keyset.Handle.WriteWithContext` to wrap a fresh DEK keyset and `keyset.ReadWithContext` to unwrap it, so the serialized cleartext keyset never exists outside Tink. The helpers format the remote error with `%v` (`keyset/handle.go`: `fmt.Errorf("keyset.Handle: decryption failed: %v", err)`), which flattens the adapter's `*kms.Error` to text; `Classify` recovers the code from that text through a small shim, `kms.CodeFromText`, pinned by a unit test that drives real Tink against the fake gate (Decisions, Tests). The alternative, calling the remote AEAD directly and parsing the keyset with `insecurecleartextkeyset`, keeps the typed error but imports the package Tink's documentation marks as dangerous and separates so that its use can be restricted and audited; Ben ranked avoiding it above avoiding string matching (pull request 8 review, 2026-09-22).
- No KEK rotation, no keyset with several keys, no Tink key-id prefix on message ciphertext (Decisions). The `kek_version` column stays at 1.

**Size and where it lands.** ≈ 290 lines changed across seven Go files and two test files, ≈ 40 lines net fewer in `internal/cmek` (the bound the plans track). One dependency and its two transitive modules (`google.golang.org/protobuf`, `golang.org/x/crypto`). Branch `claude/tink-go` cut from the M4 head (`4f2a53b`), reviewed as its own pull request into `claude/epic-rubin-kgewov`, so the M4 pull request carries it. Nothing here waits on M5.

## Files

| Path | Change | Purpose | ~Lines |
| --- | --- | --- | --- |
| `go.mod`, `go.sum` | Mod | `github.com/tink-crypto/tink-go/v2 v2.8.0` (requires Go ≥ 1.25; we are on 1.26) | +3 |
| `internal/kms/kms.go` | Mod | `KMS` interface becomes `KEK(kekID string) (tink.AEADWithContext, error)`; `DataKey` deleted; new `CodeFromText(err error) (Code, bool)` recovers a `Code` from an error whose `*Error` was flattened to text (matches `: <Code>: ` for the four codes, and `context deadline exceeded` → `Timeout`); `Code`, `Error`, `KeyEvent`, `Clock` unchanged | −12 / +26 |
| `internal/kms/fake.go` | Mod | per-KEK key is a Tink AES-256-GCM keyset (`keyset.NewHandle(aead.AES256GCMKeyTemplate())` → `aead.New`); `GenerateDataKey`/`Unwrap` replaced by one `gate` type implementing `tink.AEADWithContext` that runs the existing `begin` (fault resolve → truth → fast-fail / error rate / latency raced with ctx → enabled read after latency) and then delegates to the KEK primitive with AAD = kekID; `Revoke`/`Restore`/`SetFault`/`Truth` untouched | −45 / +40 |
| `internal/cmek/envelope.go` | Mod | `Seal`/`Open` on `Handle.prim` (`tink.AEAD`): lease check, `Encrypt`/`Decrypt` with the same `aad()`, `ErrPoison` on any decrypt error; `Envelope` loses `Nonce`; no `crypto/*` imports | 66 → ≈ 34 |
| `internal/cmek/types.go` | Mod | `Handle{DEKID, ValidUntil, prim tink.AEAD}`; `Zero()` drops the reference (Decisions); `Envelope{DEKID, Ciphertext}`; `WrappedDEK` unchanged; `Config.Keys kms.KMS` unchanged | ±10 |
| `internal/cmek/lease.go` | Mod | `dek.key *[32]byte` → `dek.prim tink.AEAD` (nil while cold); `purge()` sets `prim = nil` | ±6 |
| `internal/cmek/fetcher.go` | Mod | `result.key` → `result.prim`; `generate`: `keyset.NewHandle(aead.AES256GCMNoPrefixKeyTemplate())` → **the KMS call** `h.WriteWithContext(ctx, keyset.NewBinaryWriter(&buf), kek, aad)` → `PutDEK(buf.Bytes())` → `aead.New(h)`; `unwrap` (renew / probe / warm): **the KMS call** `keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(wrapped)), kek, aad)` → `aead.New`; `call` (tenant cap, provider semaphore, `KMSTimeout` ctx, `sentAt`) unchanged, it wraps a `func(ctx) ([]byte, error)` | ±36 |
| `internal/cmek/classify.go` | Mod | after `errors.As` misses, `kms.CodeFromText(err)` supplies the code (the Tink helpers flatten the adapter's error); the Deny set is unchanged | +4 |
| `internal/cmek/manager.go` | Mod | `handle()` and `DecryptKey` copy `d.prim` instead of `*d.key`; `Tick`'s age purge nils the primitive; nothing else | ±8 |
| `internal/queue/store.go` | Mod | `messages.nonce` column dropped; `Insert`/`Claim` bind `ciphertext` only; `Message` loses `Nonce` | −8 |
| `internal/queue/workers.go` | Mod | the one `cmek.Open` call site builds `Envelope{DEKID, Ciphertext}` | ±1 |
| `internal/world/ingest.go` | Mod | `h.Zero()` stays (a no-op that drops the primitive; Decisions) | 0 |
| `internal/cmek/cmek_test.go` | Mod | rig: `latentKMS` wraps `KEK()`; `testHandle` builds a Tink primitive; `TestEnvelope` keeps its rows (round trip, wrong tenant / msgID / dekID → `ErrPoison`, stale handle); new rows: tampered ciphertext → `ErrPoison`; **interop**: a `tink.AEAD` built independently from the same keyset decrypts our `Envelope.Ciphertext` with our AAD (proves the ciphertext is plain Tink AES-GCM, IV embedded); **wrapped form**: the `deks` blob decrypts only under its own KEK (another KEK → `AccessDenied`) and parses as a one-key AES-256-GCM keyset | ±60 |
| `internal/kms/fake_test.go` | New | the gate: fast-fail → `Unavailable`, error rate 1.0 → `Unavailable`, latency past the ctx deadline → `Timeout`, disabled KEK → `KeyDisabled` **after** the latency, truth records the call and the flip in order; unknown KEK → `AccessDenied` | 60 |
| `internal/kms/kms_test.go` | New | `TestCodeFromText`: the literal texts the shim must parse (our `kms gcp: KeyDisabled: key disabled`, Tink's `keyset.Handle: decryption failed: kms gcp: KeyDisabled: …` and `keyset.Handle: keyset.Handle: encryption failed: …` shapes, `context deadline exceeded`, and non-matches such as a plain `io.EOF` → `false`); `TestCodeThroughTink`: a real `keyset.ReadWithContext` / `WriteWithContext` against a disabled, a fast-failing and a timed-out gate, asserting the recovered code, which is the test that trips on a Tink upgrade that changes the message shape | 70 |
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
//   var buf bytes.Buffer
//   err = h.WriteWithContext(ctx, keyset.NewBinaryWriter(&buf), kek, []byte(kekID)) // the KMS call; err carries the adapter's *kms.Error as text
//   wrapped, prim = buf.Bytes(), must(aead.New(h))

// openDEK recovers the primitive from the wrapped keyset with one KMS call (renewal, probe, warm).
func (m *Manager) openDEK(ctx context.Context, kek tink.AEADWithContext, kekID string, wrapped []byte) (tink.AEAD, error)
//   h, err := keyset.ReadWithContext(ctx, keyset.NewBinaryReader(bytes.NewReader(wrapped)), kek, []byte(kekID)) // the KMS call
//   return aead.New(h)
```

```go
package kms

// CodeFromText recovers the Code of an *Error that a caller flattened to text (Tink's keyset helpers format the
// remote error with %v). It matches ": <Code>: " for the four codes and "context deadline exceeded" → Timeout;
// ok is false for any other text. Classify tries errors.As first and this second.
func CodeFromText(err error) (code Code, ok bool)
```

The audit op names stay `generate` (one wrap call after a local generation) and `unwrap`. `Classify` gains one fallback: when `errors.As` finds no `*kms.Error`, `kms.CodeFromText` supplies the code, so `KeyDisabled` and `AccessDenied` reach `Deny` through Tink exactly as they do today. The provider name is not recovered as a field; it stays in the error text, which is what the audit and the timeline print.

`internal/cmek` imports after the change: today's list minus `crypto/aes`, `crypto/cipher`, `crypto/rand`, `encoding/binary` stays (`aad`), plus `bytes`, `github.com/tink-crypto/tink-go/v2/aead`, `.../keyset`, `.../tink` (not `insecurecleartextkeyset`). CI import check (**amends ARCH** › internal/cmek): `go list -deps ./internal/cmek | grep -vE '^(golang.org/x/(sync|crypto)|google.golang.org/protobuf|github.com/tink-crypto/tink-go/v2|github.com/prabenzo/cmek/internal/(cmek|kms|logx)$|[a-z0-9./]+$)'` produces no output.

## Order of work (one lane, Claude; Ben reviews the pull request)

1. **T+0–4 spike.** `go get` the module; a throwaway test confirms three facts before any refactor: (a) the exact text `keyset.ReadWithContext` and `WriteWithContext` produce around a `*kms.Error` and around `context deadline exceeded` (what `CodeFromText` and its pinning test match), (b) a `NoPrefix` AES-256-GCM primitive's ciphertext is `12-byte IV || ct || 16-byte tag` and decrypts with the stdlib for the interop test, (c) `WriteWithContext` under a fake KEK wraps a one-key keyset to ≈ 110–140 bytes and `ReadWithContext` gives back a handle whose `aead.New` primitive decrypts what the original sealed. The test is deleted at T+4; its findings go in the TIMELOG note.
2. **T+4–12 kms.** Interface, `CodeFromText`, the `gate`, `fake_test.go`, `kms_test.go` (`TestCodeThroughTink` runs real Tink against the gate); `go test -race ./internal/kms`.
3. **T+12–24 cmek.** `types.go`, `lease.go`, `envelope.go`, `fetcher.go` (`newDEK`/`openDEK`, `result.prim`), `classify.go` (the fallback), `manager.go`; the test rig; the new `TestEnvelope` rows; `go test -race ./internal/cmek` (the 11-row walk, the extras and the concurrent cold callers must pass unchanged: they assert states, audits and call counts, none of which move).
4. **T+24–30 queue and world.** Drop `nonce`, fix `Insert`/`Claim`/`Message`, the worker's `Open` call; `go build ./... && go vet ./... && go test -race ./...`; the no-globals grep and the new import regex.
5. **T+30–36 acceptance.** Local run on `/dev/shm` at 300/s from the stream (the M4 script's blip and revocation legs): blip recovers ≤ 12 s, revocation purple ≤ 30 s with exactly one `REVOKED, N DEKs purged` line and `403 key_revoked`, Restore → green ≤ 5 s, `kms_ps.deny` non-zero during the revoke (proves the code survives Tink), `ingest_ps.internal` 0, 0 server errors. A `TestNoPlaintextAtRest` row in `store_test.go` inserts a sealed canary and asserts the stored `ciphertext` blob does not contain it (S1's mechanism, cheap to pin here).
6. **T+36–40 docs.** The amendment rows, the SPEC library line, the TIMELOG row for this branch, `README.md` › decisions table row (approval record).

Estimated ≈ 40 working minutes at the measured pace. Pushed at T+12 (kms), T+24 (cmek), T+40 (final).

## Tests

| Test | File | Asserts |
| --- | --- | --- |
| `TestEnvelope` (extended) | cmek_test.go | round trip; wrong tenant / msgID / dekID → `ErrPoison`; flipped ciphertext byte → `ErrPoison`; stale handle → `ErrLeaseExpired` from both; **interop**: `aead.New(sameHandle).Decrypt(env.Ciphertext, aad)` yields the plaintext |
| `TestWrappedDEK` (new) | cmek_test.go | after one `EncryptKey`, `mapStore` holds one `WrappedDEK` whose bytes decrypt under the tenant's KEK gate into a one-key AES-256-GCM keyset and fail under another tenant's KEK with `AccessDenied`; the walk's revoke row still sees `KeyDisabled` classified `Deny` through the Tink helpers |
| `TestCodeFromText` (new) | kms/kms_test.go | literal texts: ours, Tink's read and write shapes, `context deadline exceeded` → `Timeout`; `io.EOF` and an empty text → `false` |
| `TestCodeThroughTink` (new) | kms/kms_test.go | `keyset.ReadWithContext` against a disabled gate → `KeyDisabled`; `WriteWithContext` against a fast-failing gate → `Unavailable`; a 2 s gate under a 500 ms ctx → `Timeout`; all recovered by `CodeFromText`, none by `errors.As` (documents why the shim exists) |
| `TestGate` (new) | kms/fake_test.go | fast-fail → `Unavailable` before any latency; error rate 1 → `Unavailable`; latency 2 s vs 500 ms ctx → `Timeout`; disabled KEK → `KeyDisabled` after the latency; truth order call → flip → deny; unknown KEK → `AccessDenied` |
| `TestNoPlaintextAtRest` (new) | queue/store_test.go | a sealed canary payload's stored blob has no canary substring and no plaintext bytes |
| existing walk, extras, concurrency, world, scheduler, gate, hist | unchanged | green under `-race` with no edits beyond the rig |

## Decisions for Ben

**Status: plan drafted 2026-09-22 on branch `claude/tink-go`; awaiting Ben's approval before any code (standing process). Each bullet's first option is the recommendation.**

- **KMS round trip: Tink's `keyset.ReadWithContext`/`WriteWithContext` + `kms.CodeFromText` (decided by Ben, pull request 8 review) vs direct AEAD calls + `insecurecleartextkeyset`.** The helpers wrap the remote error with `%v`, not `%w` (`keyset/handle.go`: `fmt.Errorf("keyset.Handle: decryption failed: %v", err)`), so without a shim `Classify` would see a plain string, `KeyDisabled` and `AccessDenied` would both become Transient and REVOKED would never be entered. `CodeFromText` parses the code back out of the text; two tests pin it, one on literal strings and one that runs real Tink against the fake gate, so a Tink upgrade that changes the message shape fails `go test` rather than the revocation scenario. `go.sum` pins v2.8.0; the shape can only change when we upgrade on purpose. The alternative keeps the typed error first-hand but imports `insecurecleartextkeyset`, which Tink documents as dangerous and keeps separate so its use can be restricted and audited; Ben ranked never importing it above avoiding string matching. It stays documented here as the fallback if the shim ever proves unworkable (a Tink version that stops including the cause text).
- **Message ciphertext template: `AES256GCMNoPrefixKeyTemplate` (recommended) vs `AES256GCMKeyTemplate` (5-byte Tink key-id prefix).** One key per keyset and our own `dek_id` column already route decryption, so the prefix buys nothing and costs 5 bytes per row and a non-standard layout. The KEK keysets in the fake use the prefixed template (Tink's default for a keyset that could rotate).
- **Drop the `messages.nonce` column (recommended) vs keep it empty.** Tink embeds the IV in the ciphertext; the database is created fresh per World, so there is nothing to migrate. Keeping the column would leave a `NOT NULL` blob written empty on every insert.
- **`Handle.Zero()` stays as a reference drop (recommended) vs deleting it.** Tink holds key material in ordinary Go memory and offers no zeroization, so the explicit wipe the design promised (`Zero wipes the key bytes`) no longer exists in any form; the honest statement for the README rationale is "plaintext DEKs are dropped on purge and collected by the GC; they are not zeroized". The call site in `ingest.go` keeps compiling and the intent stays documented. Alternative: delete the method and the call. Either way `DESIGN-REFERENCE` › Concurrency ("Seal runs on a Handle copy outside every lock") still holds: the copy is an interface value.
- **Fake providers keep their own gate around a Tink AEAD (recommended) vs Tink's `testing/fakekms`.** `fakekms` embeds the key in the URI and has no faults, latency, revoke or ground truth; every behaviour the scenarios need lives in our gate anyway, so wrapping `fakekms` would add a layer and remove nothing.
- **Library list amendment.** The spec's "Libraries" row rationale ("little to review beyond our own code") changes character: we trade ≈ 130 lines of our own AES-GCM for a vetted library plus `protobuf` and `x/crypto` as transitive modules (three modules, no CGO, pure Go). Recommended: amend SPEC › Libraries and CONTEXT › allowed libraries to add `tink-go`, and say so in the README rationale as a deliberate choice. This is the one decision that is a spec change rather than a design one.
- **Scope: this branch only touches crypto.** Recommended: no KEK rotation, no keyset-level DEK rotation (our `DEKMaxMessages`/`DEKMaxAge` rotation stays a new keyset per DEK id), no `KMSEnvelopeAEAD`. Alternative: model DEK rotation as one Tink keyset per tenant with rotating primary keys, which would move the DEK cache into Tink's `keyset.Manager` and is a larger refactor with no scenario visible from it.

## Risks

- **Error classification through Tink** is the one correctness risk: the helpers flatten the adapter's error to text and `CodeFromText` parses it back. Three things hold it: `go.sum` pins the Tink version, `TestCodeThroughTink` drives real Tink against the gate so a changed message shape fails the unit tests at upgrade time, and `TestWrappedDEK`'s revoke row plus the T+30 revocation leg check the end-to-end result (REVOKED entered, `kms_ps.deny` non-zero). The `*kms.Error` text contains its code in a fixed `: <Code>: ` frame that we own, so the match does not depend on Tink's wording, only on Tink including the cause text at all.
- **Throughput.** Tink's AES-GCM is the stdlib's underneath; the primitive is built once per DEK at fetch time and cached in `dek.prim`, never per message. Expect the delivered plateau (≈ 620/s, synthetic) and the ingest cost (≈ 2 µs) unchanged; the M4 acceptance leg at T+30 reads both.
- **Dependency and build.** Three new modules, pure Go, no CGO, `go 1.25` minimum (we pin 1.26); the Dockerfile's `golang:1.26` builder needs no change. `go mod tidy` is run once and the sums committed.
- **Storage size.** The wrapped DEK grows from 60 bytes to ≈ 100–130 (serialized keyset + IV + tag); at ≤ 3 DEKs per tenant this is ≈ 400 KB per World. Message rows shrink by the 12-byte nonce column and grow by the 12-byte embedded IV: net zero.
- **Review surface.** The diff is mechanical outside `fetcher.go`'s two new helpers, `kms.CodeFromText` and the fake's gate; those four functions are where Ben's review time goes.

## Status

**Built 2026-09-23** on `claude/tink-go` (restarted from the merged plan, base `c8fc38b`), approved by Ben on PR #8 ("Plan LGTM, please proceed"). Code commit `d2628fd`, docs commit follows; Ben opens the pull request into `claude/epic-rubin-kgewov`. Numbers in `docs/TIMELOG.md` › Tink row.

**As built, where it differs from the plan above.**

- `TestCodeThroughTink` and `TestGate` use a manual clock whose `After` hands out a channel the test fires, so the "revoked during the latency" row and the timeout rows run without sleeping; the "truth records the call" wording was wrong: the truth log records key events only ([BB-11]), and the test asserts the flip.
- The fake gate maps a blob that does not verify (another KEK, another associated data) to `AccessDenied`, as `Unwrap` did; Tink itself reports it as `aead_factory: decryption failed`, which the spike surfaced.
- A zeroed `Handle` refuses `Seal` and `Open` with `ErrDEKCold` instead of sealing under nothing (the old wipe left a usable all-zero key; a nil primitive would panic).
- `TestNoPlaintextAtRest` lives in `world_test.go` (a World with no workers started keeps its rows at rest; `Handle`'s primitive is unexported, so `store_test` cannot seal).
- The wrapped keyset is 142 bytes (measured), not the 100–140 guessed; the deks table grows by ≈ 80 bytes per DEK.
- `internal/cmek` did not shrink: 774 → 789 code lines by one rule (non-blank, non-comment). The envelope lost 32 lines, but `newDEK`/`openDEK`, the classify fallback and the nil-primitive guards in `Seal`/`Open` cost more. `internal/kms` 262 → 266.
- `internal/cmek` imports: as listed, plus `bytes`; `crypto/rand`, `io` and `crypto/*` are gone. `go list -deps` also shows `tink-go/v2/insecuresecretdataaccess`, an internal Tink dependency of `keyset`, not an import of ours; `insecurecleartextkeyset` is nowhere in the graph.
- One code commit instead of two pushes: the interface change and its callers do not compile apart.
- Review (PR #9, Ben, 2026-09-23, three nits): the audit `Detail` goes through `kms.Cause`, which trims the text to the `kms <provider>: <Code>: ` frame (or the flattened deadline) so the timeline reads as before Tink, while the service log keeps Tink's full text; `CodeFromText` reads the code from the first frame in the text (the adapter's own; a later frame is upstream text echoed in `Msg`) instead of the first code word found anywhere; the DESIGN-REFERENCE fake call order no longer lists truth-log call steps.
