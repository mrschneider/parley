# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[v0.1.0]: https://github.com/mrschneider/parley/releases/tag/v0.1.0
