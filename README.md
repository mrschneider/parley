# PARLEY v0

Linguagem/protocolo de domínio para **comunicação entre agentes de IA**:
negociação com contratos híbridos (cláusulas de máquina + linguagem natural),
multi-tenant, agnóstico de transporte (HTTP, WebSocket, filas).

A serialização canônica JSON **é** o protocolo: qualquer plataforma que leia
`schema/parley-v0.schema.json` fala PARLEY.

## Modelo de confiança (agentes NÃO confiáveis)

- **id de mensagem por conteúdo** (`msg:` + sha256 da serialização canônica) →
  idempotência e detecção de replay
- **assinatura** sobre a serialização canônica (todo campo exceto `signature`),
  ed25519 (testes derivam chave do id; produção: KMS/carteira real)
- **tenant** no envelope, exigível pelo validador
- **provenance**: cadeia de ids que fundamentam a mensagem

## Contratos híbridos

- cláusula `check` — validada por máquina (caminho no body, operador, valor)
- cláusula `nl` — escrita em linguagem natural; só fica **VINCULANTE** quando o
  contraparte a lista explicitamente em `accept ... clauses "cN"`
- **ranking por peso** (`clause.weight`): dado um `request` com `expect`
  ponderadas e as propostas recebidas, `rank_proposals` ordena pela fração de
  peso contratual satisfeita. O ranking **orienta**, o requestor decide.
  Semântica idêntica em Python (referência) e Go.

## Linguagem

```parley
agent "planner-42" as PLANNER
agent "dispatcher-7" as DISPATCHER
provides "route.plan" v1
  input  origin : Location
  output route  : Route
  cost   latency_ms <= 200

PLANNER -> DISPATCHER
  request route.plan
  intent "Rota mais rápida, usuário apressado"
  with origin = "Lisboa", dest = "Porto"
  by 2026-10-22T15:00:00Z
  expect confidence > 0.85
  expect route.eta_min <= 150 w 0.9
  expect nl "Evitar obras nas próximas 24h" w 0.9
  fallback -> "dispatcher-backup"
```

Veja `examples/negociacao.parley` (request → propose → accept seletivo).

## Componentes

| Caminho | O que é |
|---|---|
| `parley.py` | Referência Python: parser, canonical JSON, assinatura, validação, ranking, thread |
| `parley_lsp.py` | **LSP** para `.parley` (zero deps, stdio) + `--check` one-shot |
| `test_parley.py` | Testes da referência Python (13) |
| `test_lsp.py` | Testes do LSP com cliente JSON-RPC real (7) |
| `schema/parley-v0.schema.json` | JSON Schema da mensagem canônica |
| `sdk/go/` | SDK Go (parser, validação, ranking, registry, state, transport) |
| `vscode/` | Extensão VS Code: syntax highlighting, LSP, snippets |

## Badges

[![CI](https://github.com/mrschneider/parley/actions/workflows/ci.yml/badge.svg)](https://github.com/mrschneider/parley/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Rodando

```sh
# referência
python3 test_parley.py
python3 parley.py examples/negociacao.parley --thread

# LSP
python3 test_lsp.py
python3 parley_lsp.py --check examples/negociacao.parley   # one-shot
python3 parley_lsp.py                                       # server stdio

# Go
cd sdk/go && go test ./...
```

## Licença

MIT — veja [LICENSE](LICENSE).

## LSP — capacidades

- `textDocumentSync` full → `publishDiagnostics` a cada open/change
  - sintaxe (varredura tolerante: **todos** os erros, não só o primeiro)
  - semântica: `@msgN` fora do alcance, fallback para agente não declarado,
    peso de cláusula fora de (0,1], `request` sem target, cláusula inexistente no pai
- `completion` — keywords por contexto (top / capability / mensagem),
  nomes de capabilities no ato, ids de agentes no `fallback`
- `hover` — docs de keywords, agentes, capabilities, referências `@msgN`
- `documentSymbol` — agentes (com provides), capabilities, mensagens

**Regra numérica canônica** (protocolo): números serializam por *valor* —
inteiros puros (`150`, nunca `150.0`), não-inteiros na menor representação
round-trip (`0.85`). É ela que torna ids/assinaturas independentes de plataforma.
