# Contributing to PARLEY

Thanks for your interest. PARLEY is a small, spec-first project: the canonical
JSON serialization **is** the protocol, and both implementations (Python
reference, Go SDK) must stay semantically identical.

## Development setup

```sh
# Python (stdlib only — no venv required for the reference implementation)
python3 test_parley.py
python3 test_lsp.py

# Go SDK
cd sdk/go && go test ./...
```

Any Python ≥ 3.10 works; the reference uses zero third-party dependencies.

## Rules

1. **Protocol changes are cross-implementation.** A change to canonical
   serialization, ranking semantics, or the schema must be implemented and
   tested in BOTH Python and Go in the same PR, with the interop fixtures
   (`sdk/go/testdata/`) updated.
2. **Canonical numbers by value.** Integers serialize as integers (`150`, never
   `150.0`); non-integers use the shortest round-trip representation. Never add
   locale- or platform-dependent formatting.
3. **Diagnostics tolerate everything.** The LSP must report *all* errors in a
   document, not stop at the first.
4. **No new dependencies** in the Python reference or the LSP without discussion.

## Pull request checklist

- [ ] Tests pass locally: `python3 test_parley.py && python3 test_lsp.py`
- [ ] Go tests pass: `cd sdk/go && go test ./...`
- [ ] If the protocol changed: both implementations + schema + fixtures updated
- [ ] `CHANGELOG.md` has an entry under *Unreleased*
- [ ] No secrets, tokens, or environment-specific identifiers in the diff
