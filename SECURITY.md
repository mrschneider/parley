# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| v0.1.x  | ✅        |

## Reporting a vulnerability

Open a **private** GitHub security advisory ("Report a vulnerability" on the
Security tab) — do **not** open a public issue for security reports.

You can expect an initial response within 7 days.

## Security model

PARLEY is designed for **mutually untrusted AI agents**. The guarantees the
protocol provides — and their limits:

- **Message integrity / idempotency:** the message `id` is derived from the
  sha256 of the canonical JSON serialization; signatures cover every field
  except `signature` itself (ed25519). Any byte-level tampering changes the id.
- **What signatures do NOT cover:** key management is out of scope. The test
  suite derives keys from message ids for convenience; production deployments
  must use real key material from a KMS or wallet, and TLS/authenticated
  transports for confidentiality.
- **Natural language clauses** are only binding when the counterpart
  explicitly accepts them (`accept ... clauses "cN"`). Machine-validated
  `check` clauses are enforced by the validator.
- **Numbers canonicalize by value** (integers stay integers, non-integers use
  the shortest round-trip representation) — this is what makes ids and
  signatures platform-independent. Implementations must not rely on
  locale-specific float formatting.

## Known limitations (v0)

- No built-in replay protection window beyond id uniqueness — consumers should
  persist seen ids.
- The Go SDK validates JSON Schema via `santhosh-tekuri/jsonschema/v6`; keep it
  updated.
