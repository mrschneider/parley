## Checklist

- [ ] Local tests green: `python3 test_parley.py && python3 test_lsp.py`
- [ ] Go green: `cd sdk/go && go test ./... && go vet ./...`
- [ ] If the protocol changed: Python **and** Go implemented in the same PR, with
      fixtures/schemas updated (rule from CONTRIBUTING.md)
- [ ] Canonical number rule preserved (no locale-dependent formatting)
- [ ] `CHANGELOG.md` updated
- [ ] No secrets, tokens, or environment-specific identifiers in the diff
