# PARLEY v0

Linguagem/protocolo de domínio para **comunicação entre agentes de IA**:
negociação com contratos híbridos (cláusulas de máquina + linguagem natural),
multi-tenant, agnóstico de transporte (HTTP, WebSocket, filas).

A serialização canônica JSON **é** o protocolo: qualquer plataforma que leia
`schema/parley-v0.schema.json` (ou `schema/parley-v1.schema.json`, que adiciona
cláusulas compostas `all_of`/`any_of`) fala PARLEY.

## Modelo de confiança (agentes NÃO confiáveis)

- **id de mensagem por conteúdo** (`msg:` + sha256 da serialização canônica) →
  idempotência e detecção de replay
- **assinatura** sobre a serialização canônica (todo campo exceto `signature`),
  ed25519
  - **modo teste**: a chave é derivada do id (conveniente, NÃO seguro —
    qualquer um que conheça o id forja a assinatura)
  - **modo produção**: `KeyStore` (Python e Go) gera/carrega pares ed25519
    reais; a pública vai para o **Agent Card** publicado no **Registry**, e o
    receptor verifica via `card.Pub` (`Registry.VerifyMessageWithCard`) —
    a âncora de confiança é o card registrado, não a derivação
- **tenant** no envelope, exigível pelo validador
- **provenance**: cadeia de ids que fundamenta a mensagem (usada também
  para filiação de `accept` sem `parent` explícito)

## Contratos híbridos

- cláusula `check` — validada por máquina (caminho no body, operador, valor)
- **v1**: `check` também aceita composição — `all_of` (E) e `any_of` (OU)
  recursivamente sobre folhas (`schema/parley-v1.schema.json`); v0 rejeita
  composição explicitamente
- cláusula `nl` — escrita em linguagem natural; só fica **VINCULANTE** quando a
  contraparte a lista explicitamente em `accept ... clauses "cN"`
- **ranking por peso** (`clause.weight`): dado um `request` com `expect`
  ponderadas e as propostas recebidas, `rank_proposals` ordena pela fração de
  peso contratual satisfeita. O ranking **orienta**, o requestor decide.
  Semântica idêntica em Python (referência) e Go.
- **acordo materializado** (`build_agreement`/`BuildAgreement`): ao assentar,
  a negociação vira um recibo canônico de conteúdo endereçado (`agreement:` +
  sha256) com vencedor, corpo aceito e cláusulas binding — assinado por
  requestor **e** provider, verificável sem replayer a thread (Go: ver
  `agreement.go`; Python: `build_agreement`/`sign_agreement`/`verify_agreement`)

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
| `parley.py` | Referência Python: parser, canonical JSON, assinatura, `KeyStore` (ed25519 real), validação (v0/v1), ranking, thread, acordos |
| `parley_lsp.py` | **LSP** para `.parley` (zero deps, stdio) + `--check` one-shot |
| `test_parley.py` | Testes da referência Python (16) |
| `test_lsp.py` | Testes do LSP com cliente JSON-RPC real (7) |
| `schema/parley-v0.schema.json` | JSON Schema da mensagem canônica (v0) |
| `schema/parley-v1.schema.json` | v1: adiciona `check` composto (`all_of`/`any_of`) |
| `sdk/go/` | SDK Go: parser, validação, ranking, registry, state, keystore, agreement, transports |
| `sdk/go/transport/` | Queue + HTTP + **WebSocket** (gorilla), Inbox (retenção), Supervisor (deadline/fallback) |
| `vscode/` | Extensão VS Code: syntax highlighting, LSP, snippets |

## Runtime de produção (Go)

Os três pilares se conectam num caminho único (verificado em
`transport/runtime_test.go` e no demo `go run ./demo`):

1. **KeyStore** gera/carrega pares ed25519 reais (arquivo JSON, chmod 0600);
2. **Registry** publica o Agent Card assinado e serve de âncora de confiança
   (`WithCardRegistry` no Inbox);
3. **Supervisor** vigia deadline/fallback e o `CompleteFallbackSink`
   re-alvoa, re-assina e entrega a escalada automaticamente;
4. **Agreement** materializa o desfecho assinado por ambos os lados.

Persistência: escritas são **atômicas e serializadas entre processos**
(`AtomicWriteFile`/`lockedWrite` — lock-irmão `.lock` + temp/rename).
Inbox aceita política de retenção (`WithRetention(maxAge, maxRecords)`), com
evolução automática após cada entrega e `Sweep()` manual.

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
