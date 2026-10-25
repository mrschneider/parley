# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed
- **Repository translated to English**: docs, templates, code comments,
  diagnostics, demo output, tests and fixtures. The example was renamed
  `examples/negociacao.parley` -> `examples/negotiation.parley` (all
  references updated, CI smoke step included). Interop fixtures
  (`interop-py`, `interop-id`, `interop-ranking`) regenerated from
  English content with matching canonical bytes, content ids and
  signatures; Python/Go diagnostics use the same English glossary.

## [v0.2.0] - 2026-10-25

### Added
- **Schema v1** (`schema/parley-v1.schema.json`): composite machine clauses
  `all_of`/`any_of` (recursive); v0 explicitly rejects composites. Validated
  by both implementations.
- **Production keys**: `KeyStore` in Python and Go — real ed25519 pairs,
  chmod 0600 persistence, signing with the raw public key as key id.
  `verify`/`Verify` accept both test-derived and raw-key signatures.
- **Registry trust anchor**: `Registry.VerifyMessageWithCard` + inbox option
  `WithCardRegistry` — production verification path (card.Pub, revocation
  checked).
- **Complete fallback runtime**: `CompleteFallbackSink` retargets,
  re-assigns the content id, re-signs with the KeyStore, and delivers the
  escalation; end-to-end test ties Registry + Inbox + Supervisor together.
- **WebSocket transport**: `WSServer`/`WSSend` (gorilla/websocket) — same
  canonical JSON envelope as HTTP/queue, one text frame per message.
- **Agreement receipt**: `build_agreement`/`BuildAgreement` materializes the
  settlement (winning offer, accepted body, binding NL clauses) as a
  content-addressed artifact signed by both requestor and provider;
  tampering invalidates the id. Filiation via `parent` or `provenance`.
- **Inbox retention**: `WithRetention(maxAge, maxRecords)` bounds store and
  replay sets; automatic sweep per delivery + manual `Sweep()`.
- **Cross-process write locking**: `LockedWrite`/`AtomicWriteFile` (exclusive
  `.lock` sibling, stale-lock break at 5s, temp+rename) used by Inbox,
  Registry, and KeyStore persistence (Python mirrors the same recipe).

### Changed
- `Registry.Marshal`/`Unmarshal` round-trip canonically (triples).
- README: production trust model, v1 composites, runtime/production section.

## [v0.1.0] - 2026-10-25

### Added
- Reference implementation in Python: parser, canonical JSON serialization,
  ed25519 signatures, validation, weighted contract ranking, negotiation threads.
- JSON Schema for the canonical message (`schema/parley-v0.schema.json`).
- Language Server Protocol server for `.parley` files (zero dependencies,
  stdio): diagnostics (syntax + semantics), completion, hover, document symbols,
  plus a one-shot `--check` mode.
- Go SDK (`sdk/go`): parser, validation, ranking (bit-identical semantics with
  the Python reference), agent registry, negotiation state machine, HTTP/WebSocket
  transport, `parleygo` CLI.
- VS Code extension: syntax highlighting, LSP client, snippets.
- Test suites: 13 Python reference tests, 7 LSP tests (real JSON-RPC client),
  Go `go test ./...` including Python↔Go interop fixtures.

[Unreleased]: https://github.com/mrschneider/parley/compare/v0.2.0...HEAD
[v0.2.0]: https://github.com/mrschneider/parley/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/mrschneider/parley/releases/tag/v0.1.0
