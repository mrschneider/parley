#!/usr/bin/env python3
"""PARLEY v0 — demo de negociação de ponta a ponta (multi-tenant, não confiáveis)."""
import hashlib
import json
import sys
from datetime import datetime, timezone

from parley import parse_text, finalize, sign, validate, verify, rank_proposals, thread_from, canonical_bytes

NOW = datetime(2026, 10, 22, 12, 0, 0, tzinfo=timezone.utc)

def banner(t):
    print("\n" + "=" * 74 + f"\n{t}\n" + "=" * 74)

def show(label, m, extra=None):
    print(f"\n--- {label} ---")
    slim = {k: v for k, v in m.items() if k != "signature"}
    if m.get("signature"):
        slim["signature"] = {"alg": m["signature"]["alg"], "key": m["signature"]["key"],
                             "sig": m["signature"]["sig"][:24] + "..."}
    print(json.dumps(slim, indent=2, ensure_ascii=False))
    if extra:
        print(extra)

src = open("examples/negociacao.parley").read()
doc = parse_text(src)
msgs = finalize(doc)
msgs = [sign(m, m["from"]["id"], m.get("tenant", "default")) for m in msgs]
m1, m2, m3 = msgs

banner("1) VALIDAÇÃO (mensagem 1: request do PLANNER)")
r1 = validate(m1, parent=None, now=NOW)
print(json.dumps(r1, indent=2, ensure_ascii=False))

banner("2) VALIDAÇÃO (mensagem 2: propose do DISPATCHER contra o contrato da msg 1)")
r2 = validate(m2, parent=m1, now=NOW)
print(json.dumps(r2, indent=2, ensure_ascii=False))

banner("3) VALIDAÇÃO (mensagem 3: accept seletivo do PLANNER)")
r3 = validate(m3, parent=m1, now=NOW)
print(json.dumps(r3, indent=2, ensure_ascii=False))

banner("4) TAMPER PROOF: mensagem 2 com corpo adulterado")
m2_bad = json.loads(json.dumps(m2))
m2_bad["body"]["eta_min"] = 240  # viola 'route.eta_min <= 150'? não — mas conf 0.93 ok
m2_bad["confidence"] = 0.5       # viola 'confidence > 0.85'
ok, why = verify(m2_bad)
r2b = validate(m2_bad, parent=m1, now=NOW)
print(f"assinatura: {ok} ({why})")
print(json.dumps(r2b["clauses"], indent=2, ensure_ascii=False))

banner("5) RANKING POR WEIGHT: duas propostas concorrentes para o MESMO pedido")
def _mk(frm, ts, body, conf, clauses=None):
    m = {"v": 0, "from": {"id": frm}, "to": {"id": "planner-42"}, "ts": ts,
         "act": "propose", "target": "route.plan", "parent": m1["id"],
         "tenant": "acme", "body": body, "confidence": conf}
    if clauses:
        m["clauses"] = clauses
    tmp = {k: v for k, v in m.items() if k not in ("id", "signature")}
    m["id"] = "msg:" + hashlib.sha256(canonical_bytes(tmp)).hexdigest()[:16]
    return sign(m, frm, "acme")
# A: conf 0.93, eta 142, NÃO aceita a cláusula NL -> penalizada (score 0)
# B: conf 0.95, eta 100, ACEITA a NL (c3) -> satisfaz tudo (score 1.0)
pa = _mk("dispatcher-7", "2026-10-22T12:01:00Z",
         {"route": {"eta_min": 142}}, 0.93)
pb = _mk("dispatcher-8", "2026-10-22T12:02:00Z",
         {"route": {"eta_min": 100}}, 0.95, clauses=["c3"])
rk = rank_proposals(m1, [pa, pb])
for e in rk["entries"]:
    print(f"  {e['proposal']['from']['id']}: score={e['score']:.2f}  "
          f"ok={e['satisfied']} viol={e['violations']}")
print(f"topo: {rk['top']['proposal']['from']['id']} (score {rk['top']['score']:.2f}) — "
      f"B aceita a cláusula NL (binding) e satisfaz todas as checks")

banner("6) THREAD: máquina de estados dos acts sobre a negociação")
th = thread_from(8, msgs)
for s in th.trace:
    print(f"  {s['act']:<8} {s['id']}  -> estado {s['state']}")
print(f"estado final: {th.state} (accept encerrou a negociação; "
      f"ofertas: {th.offer_count}, cap: {th.cap})")

banner("7) RESUMO")
print(f"schema canônico:  3 mensagens validadas contra schema v0")
print(f"assinaturas:      {r1['signature_ok']}, {r2['signature_ok']}, {r3['signature_ok']} (algs: {m1['signature']['alg']})")
cl2 = r2["clauses"]
nl_c = [c for c in cl2 if "nl" in c][0]
print(f"cláusula NL:      binding={nl_c['binding']} ({nl_c['detail']}) — proposta não listou clauses")
cl3 = [c for c in r3["clauses"] if "nl" in c][0]
print(f"cláusula NL:      binding={cl3['binding']} ({cl3['detail']}) — aceite explícito listou 'c3'")
print(f"cláusulas check na proposta: {[(c['id'], c['ok']) for c in cl2 if 'check' in c]}")
print(f"tamper detectado: {not r2b['signature_ok']}")
print(f"ranking:          topo={rk['top']['proposal']['from']['id']} score={rk['top']['score']:.2f} "
      f"(weight das cláusulas decide)")
print(f"thread:           {th.state} após {[s['act'] for s in th.trace]}")
sys.exit(0 if (r1["schema_ok"] and r2["signature_ok"] and r3["signature_ok"]
               and not r2b["signature_ok"]
               and rk["top"]["proposal"]["from"]["id"] == "dispatcher-8"
               and th.state == "settled") else 1)
