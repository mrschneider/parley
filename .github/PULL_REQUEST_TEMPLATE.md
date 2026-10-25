## Checklist

- [ ] Testes locais verdes: `python3 test_parley.py && python3 test_lsp.py`
- [ ] Go verdes: `cd sdk/go && go test ./... && go vet ./...`
- [ ] Se o protocolo mudou: Python **e** Go implementados na mesma PR, com
      fixtures/schemas atualizados (regra de CONTRIBUTING.md)
- [ ] Regra numérica canônica preservada (nada de formatação por locale)
- [ ] `CHANGELOG.md` atualizado
- [ ] Sem segredos, tokens ou identificadores do ambiente no diff