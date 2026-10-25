#!/usr/bin/env python3
"""
PARLEY v0 — linguagem/protocolo de comunicação entre agentes de IA.
Linguagem de domínio específica, agnóstica de plataforma (HTTP, WebSocket, filas).

Modelo de confiança (multi-tenant, agentes NÃO confiáveis):
  - id de mensagem endereçado por conteúdo (sha256) -> idempotência e detecção de replay
  - assinatura sobre a serialização canônica (todo campo exceto 'signature')
  - 'tenant' no envelope, exigível pelo validador
  - provenance: cadeia de ids que fundamentam a mensagem
Contratos híbridos:
  - cláusula 'check': validada por máquina (caminho no body, operador, valor)
  - cláusula 'nl':   escrita em linguagem natural; só se torna VINCULANTE quando o
                     contraparte a lista explicitamente em ACCEPT (CLAUSES ...)
"""
from __future__ import annotations
import base64
import hashlib
import hmac
import json
import re
import sys
from datetime import datetime, timezone

# ---------------------------------------------------------------- ed25519 / hmac
try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    _HAVE_ED25519 = True
except Exception:
    _HAVE_ED25519 = False

def _derive_key(agent_id: str, tenant: str = "default"):
    """Chave de teste determinística derivada da identidade.
    Produção: usar KMS / carteira de chaves real — NUNCA derivar de id."""
    seed = hashlib.sha256(f"{tenant}:{agent_id}".encode()).digest()
    if _HAVE_ED25519:
        return ("ed25519", Ed25519PrivateKey.from_private_bytes(seed))
    return ("hmac-sha256", seed)

# ---------------------------------------------------------------- serialização
#
# CANONICAL NUMBER RULE (protocolo): valores são serializados por VALOR, não
# por token — inteiros como inteiros puros ("150", nunca "150.0" nem "1.0"),
# não-inteiros com a menor representação round-trip ("0.85"). Python e Go
# DEVEM seguir esta regra; é ela que torna os bytes canônicos (e portanto
# ids e assinaturas) independentes de plataforma.

def canonical_json(v) -> str:
    """Serialização canônica de qualquer valor JSON (regra numérica canônica)."""
    if v is None:
        return "null"
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, int):
        return str(v)
    if isinstance(v, float):
        if v == int(v) and abs(v) < 1e16:
            return str(int(v))
        return repr(v)
    if isinstance(v, str):
        return json.dumps(v, ensure_ascii=False)
    if isinstance(v, dict):
        return "{" + ",".join(
            json.dumps(k, ensure_ascii=False) + ":" + canonical_json(v[k])
            for k in sorted(v)) + "}"
    if isinstance(v, (list, tuple)):
        return "[" + ",".join(canonical_json(x) for x in v) + "]"
    raise TypeError(f"valor não serializável canonicamente: {type(v)!r}")

def canonical_bytes(msg: dict) -> bytes:
    """Serialização canônica: tudo exceto 'signature', chaves ordenadas, compacto."""
    body = {k: v for k, v in msg.items() if k != "signature"}
    return canonical_json(body).encode("utf-8")

def sign(msg: dict, agent_id: str, tenant: str = "default") -> dict:
    alg, key = _derive_key(agent_id, tenant)
    data = canonical_bytes(msg)
    if alg == "ed25519":
        sig = key.sign(data)
    else:
        sig = hmac.new(key, data, hashlib.sha256).digest()
    out = dict(msg)
    out["signature"] = {"alg": alg, "key": f"parley-test:{agent_id}",
                        "sig": base64.b64encode(sig).decode()}
    return out

def verify(msg: dict) -> tuple[bool, str]:
    sig = msg.get("signature")
    if not sig:
        return False, "sem assinatura"
    alg, key_id = sig.get("alg"), sig.get("key", "")
    agent_id = key_id.split(":", 1)[1] if ":" in key_id else key_id
    tenant = msg.get("tenant", "default")
    data = canonical_bytes(msg)
    if alg == "ed25519" and _HAVE_ED25519:
        alg2, key = _derive_key(agent_id, tenant)
        try:
            key.public_key().verify(base64.b64decode(sig["sig"]), data)
            return True, "assinatura válida (ed25519)"
        except Exception as e:
            return False, f"assinatura inválida: {e}"
    if alg == "hmac-sha256":
        alg2, key = _derive_key(agent_id, tenant)
        expect = hmac.new(key, data, hashlib.sha256).digest()
        if hmac.compare_digest(expect, base64.b64decode(sig["sig"])):
            return True, "assinatura válida (hmac-sha256)"
        return False, "assinatura inválida (hmac)"
    return False, f"algoritmo não suportado: {alg}"

# ---------------------------------------------------------------- lexer/parser
ACTS = {"inform", "ask", "request", "propose", "accept", "reject", "counter",
        "commit", "delegate", "verify", "cancel"}
OPS = {"<": "lt", "<=": "lte", ">": "gt", ">=": "gte", "==": "eq", "=": "eq",
       "in": "in", "not_null": "not_null", "~": "regex"}
HEADER_RE = re.compile(r"^([A-Za-z_][\w-]*)\s*->\s*([A-Za-z_][\w-]*)$")

def parse_value(s: str):
    s = s.strip()
    if s.startswith('"'):
        return json.loads(s)
    if s.startswith("[") or s.startswith("{"):
        try:
            return json.loads(s)
        except json.JSONDecodeError:
            if s.startswith("["):  # lista solta: ["a","b"] ou [a, b, c]
                inner = s[1:-1]
                return [parse_value(x) for x in split_top(inner)]
            raise
    if s.lower() == "true":
        return True
    if s.lower() == "false":
        return False
    if re.fullmatch(r"-?\d+", s):
        return int(s)
    if re.fullmatch(r"-?\d+\.\d+", s):
        return float(s)
    return s

def split_top(s: str, sep: str = ",") -> list[str]:
    parts, depth, cur, in_str = [], 0, [], False
    for ch in s:
        if ch == '"':
            in_str = not in_str
        if not in_str:
            if ch in "[{":
                depth += 1
            elif ch in "]}":
                depth -= 1
        if ch == sep and depth == 0 and not in_str:
            parts.append("".join(cur)); cur = []
        else:
            cur.append(ch)
    if "".join(cur).strip():
        parts.append("".join(cur))
    return [p.strip() for p in parts if p.strip()]

class ParleyDoc:
    def __init__(self):
        self.agents: dict[str, dict] = {}      # alias -> {id, provides: [...]}
        self.capabilities: dict[str, dict] = {} # "name" -> {version, inputs, outputs, cost, default_confidence}
        self.messages: list[dict] = []

def _new_msg(doc: ParleyDoc, frm: str, to: str) -> dict:
    a = doc.agents[frm]
    m = {
        "v": 0,
        "from": {"id": a["id"]},
        "to": {"id": doc.agents[to]["id"]},
        "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }
    doc.messages.append(m)
    return m

def parse_text(src: str) -> ParleyDoc:
    doc = ParleyDoc()
    cur_agent = None      # alias
    cur_prov = None       # name da capability em declaração
    cur_msg = None        # mensagem em construção
    pending_act = None    # ato + target da linha de ato

    for ln, raw in enumerate(src.splitlines(), 1):
        line = raw.split("#", 1)[0].rstrip() if not raw.lstrip().startswith("#") else ""
        if not line.strip():
            continue
        s = line.strip()
        first = s.split(None, 1)[0].upper()

        # ---- declarações de capacidade -------------------------------------
        if first == "AGENT":
            m = re.match(r'^AGENT\s+"([^"]+)"\s+AS\s+([A-Za-z_][\w-]*)$', s, re.I)
            if not m:
                raise SyntaxError(f"ln {ln}: AGENT malformado: {s}")
            cur_agent = m.group(2)
            doc.agents[cur_agent] = {"id": m.group(1), "provides": []}
            continue
        if first == "PROVIDES":
            m = re.match(r'^PROVIDES\s+"?([a-z0-9_.]+)"?\s+V(\d+)$', s, re.I)
            if not m or not cur_agent:
                raise SyntaxError(f"ln {ln}: PROVIDES malformado: {s}")
            cur_prov = m.group(1)
            cap = doc.capabilities.setdefault(cur_prov, {
                "version": int(m.group(2)), "inputs": [], "outputs": [],
                "cost": [], "default_confidence": None, "by": cur_agent,
            })
            cap["version"] = int(m.group(2))
            doc.agents[cur_agent]["provides"].append(cur_prov)
            continue
        if first in ("INPUT", "OUTPUT") and cur_prov:
            m = re.match(r'^INPUT\s+(\w+)\s*:\s*(\w+)$|^OUTPUT\s+(\w+)\s*:\s*(\w+)$', s, re.I)
            if not m:
                raise SyntaxError(f"ln {ln}: INPUT/OUTPUT malformado: {s}")
            kind = "inputs" if first == "INPUT" else "outputs"
            g = m.groups()
            name, typ = (g[0], g[1]) if kind == "inputs" else (g[2], g[3])
            doc.capabilities[cur_prov][kind].append({"name": name, "type": typ.upper()})
            continue
        if first == "COST" and cur_prov:
            m = re.match(r'^COST\s+(\w+)\s*(<=|>=|<|>|==)\s*(.+)$', s, re.I)
            if not m:
                raise SyntaxError(f"ln {ln}: COST malformado: {s}")
            doc.capabilities[cur_prov]["cost"].append(
                {"field": m.group(1), "op": OPS[m.group(2)], "value": parse_value(m.group(3))})
            continue
        if first == "DEFAULT_CONFIDENCE" and cur_prov:
            doc.capabilities[cur_prov]["default_confidence"] = parse_value(s.split(None, 1)[1])
            continue

        # ---- mensagens -------------------------------------------------------
        hm = HEADER_RE.match(s)
        if hm and hm.group(1) in doc.agents and hm.group(2) in doc.agents:
            cur_msg = _new_msg(doc, hm.group(1), hm.group(2))
            continue
        if cur_msg is None:
            raise SyntaxError(f"ln {ln}: linha fora de contexto (agente/mensagem): {s}")
        m = cur_msg

        if first in {a.upper() for a in ACTS}:
            rest = s.split(None, 1)[1].strip() if " " in s else ""
            m["act"] = first.lower()
            if rest:
                m["target"] = rest.strip('"')
            continue
        if first == "INTENT":
            m["intent"] = parse_value(s.split(None, 1)[1])
            continue
        if first == "WITH":
            m.setdefault("body", {})
            for pair in split_top(s.split(None, 1)[1]):
                k, _, v = pair.partition("=")
                m["body"][k.strip()] = parse_value(v)
            continue
        if first == "BY":
            m["deadline"] = s.split(None, 1)[1].strip()
            continue
        if first == "NOTE":
            m["note"] = parse_value(s.split(None, 1)[1])
            continue
        if first == "CONFIDENCE":
            m["confidence"] = parse_value(s.split(None, 1)[1])
            continue
        if first == "TENANT":
            m["tenant"] = parse_value(s.split(None, 1)[1])
            continue
        if first == "PARENT":
            m["parent"] = s.split(None, 1)[1].strip().strip('"')
            continue
        if first == "PROVENANCE":
            m["provenance"] = [x.strip().strip('"') for x in split_top(s.split(None, 1)[1])]
            continue
        if first == "FALLBACK":
            rest = s.split("->", 1)[1].strip()
            fb_id = rest.split(None, 1)[0].strip('"')
            m["fallback"] = {"to": {"id": fb_id}}
            parts = rest.split(None, 2)
            if len(parts) >= 2 and parts[1].startswith('"'):
                m["fallback"]["reason"] = parse_value(parts[1])
            continue
        if first == "EXPECT":
            rest = s[len("EXPECT"):].strip()
            clause: dict = {}
            weight = 1.0
            wm = re.search(r"\s+W\s+(\d+(\.\d+)?)\s*$", rest, re.I)
            if wm:
                weight = float(wm.group(1))
                rest = rest[: wm.start()]
            if rest.upper().startswith("NL "):
                clause["nl"] = parse_value(rest[3:])
            else:
                cm = re.match(r"^([A-Za-z_][\w.]*)\s*(<=|>=|<|>|==|in|not_null|~)\s*(.+)$", rest)
                if not cm:
                    raise SyntaxError(f"ln {ln}: EXPECT malformado: {s}")
                clause["check"] = {"field": cm.group(1), "op": OPS[cm.group(2)],
                                   "value": parse_value(cm.group(3))}
            clause["weight"] = weight
            m.setdefault("expect", []).append(clause)
            continue
        if first == "CLAUSES":
            m["clauses"] = [x.strip().strip('"') for x in split_top(s.split(None, 1)[1])]
            continue
        raise SyntaxError(f"ln {ln}: token desconhecido: {first!r} em {s}")

    # ids de cláusula estáveis + ids de mensagem endereçados por conteúdo
    for m in doc.messages:
        for i, c in enumerate(m.get("expect", []), 1):
            c.setdefault("id", f"c{i}")
        m["_signer"] = m["from"]["id"]
    return doc

def finalize(doc: ParleyDoc) -> list[dict]:
    """Gera ids de mensagem endereçados por conteúdo (idempotência/replay)."""
    for m in doc.messages:
        tmp = {k: v for k, v in m.items() if k not in ("id", "signature")}
        h = hashlib.sha256(canonical_bytes(tmp)).hexdigest()[:16]
        m["id"] = f"msg:{h}"
        m.pop("_signer", None)
    # resolve referências @msgN -> id da N-ésima mensagem (correlação legível)
    for m in doc.messages:
        if m.get("parent", "").startswith("@msg"):
            n = int(m["parent"][4:])
            m["parent"] = doc.messages[n - 1]["id"]
        if m.get("provenance"):
            m["provenance"] = [doc.messages[int(p[4:]) - 1]["id"] if p.startswith("@msg") else p
                               for p in m["provenance"]]
    return doc.messages

# ---------------------------------------------------------------- validação
def get_path(obj: dict, path: str):
    cur = obj
    for part in path.split("."):
        if isinstance(cur, dict) and part in cur:
            cur = cur[part]
        else:
            return None
    return cur

def check_clause(clause: dict, msg: dict) -> tuple[bool, str]:
    chk = clause.get("check")
    if not chk:
        return True, "nl (sem verificação de máquina)"
    field, op, val = chk["field"], chk["op"], chk.get("value")
    actual = msg.get("confidence") if field == "confidence" else get_path(msg.get("body", {}), field)
    if op == "not_null":
        ok = actual is not None
    elif op == "in":
        ok = actual in val
    elif op == "regex":
        ok = actual is not None and re.search(val, str(actual)) is not None
    else:
        if actual is None:
            return False, f"campo ausente: {field}"
        ok = {"lt": actual < val, "lte": actual <= val, "gt": actual > val,
              "gte": actual >= val, "eq": actual == val}[op]
    return ok, f"{field}={actual!r} ({op} {val!r})"

# ---------------------------------------------------------------- ranking
#
# clause.weight: "How much this clause matters to the sender (drives
# negotiation ranking)". rank_proposals implementa a promessa: dado um
# request com cláusulas 'expect' ponderadas e as propostas recebidas
# (propose/counter), devolve a ordenação pela fração de peso contratual
# satisfeita. Mesmas semânticas dos contratos híbridos: reportado, nunca
# rejeição dura — o ranking ORIENTA, o requestor decide.
#
#   check ok: +weight | check violada: -weight (violation)
#   nl não aceita: -weight (não vinculante) | nl aceita: +weight (binding)
#   weight <= 0: inativa (não pontua)
#   score = satisfeito/total (0..1); 0 quando há violação com peso > 0
#
# Ordem determinística: score desc, confidence desc, id asc.

OFFER_ACTS = ("propose", "counter")

def rank_proposals(parent: dict, proposals: list[dict]) -> dict:
    out = {"parent_id": parent.get("id") if parent else None,
           "total": 0.0, "entries": [], "top": None}
    if not parent:
        return out
    total = sum(c.get("weight", 0.0) for c in parent.get("expect", [])
                if c.get("weight", 0.0) > 0)
    out["total"] = total
    for p in proposals or []:
        if not p or p.get("act") not in OFFER_ACTS:
            continue
        e = {"proposal": p, "score": 0.0, "satisfied": [], "violations": [], "detail": []}
        satisfied, any_violation = 0.0, False
        for c in parent.get("expect", []):
            w = c.get("weight", 0.0)
            if w <= 0:
                continue  # cláusula inativa
            if "nl" in c:
                cid = c.get("id", "?")
                ok = cid in set(p.get("clauses", []))
                detail = "nl aceita explicitamente (binding)" if ok \
                    else "nl não aceita explicitamente -> não vinculante (penalidade)"
            else:
                cid = c.get("id", "?")
                ok, detail = check_clause(c, p)
            e["detail"].append({"id": cid, "weight": w, "ok": ok, "detail": detail})
            if ok:
                satisfied += w
                e["satisfied"].append(cid)
            else:
                any_violation = True
                e["violations"].append(cid)
        if total > 0 and not any_violation:
            e["score"] = satisfied / total
        out["entries"].append(e)
    def _key(e):
        conf = e["proposal"].get("confidence") or 0.0
        return (-e["score"], -conf, e["proposal"].get("id", ""))
    out["entries"].sort(key=_key)
    if out["entries"]:
        out["top"] = out["entries"][0]
    return out

# ---------------------------------------------------------------- thread
#
# Máquina de estados dos acts: uma thread de negociação é aberta por um
# request (com target) e percorre os acts segundo uma gramática mínima,
# com terminal explícito (accept/commit/reject/cancel) e loops de
# negociação (propose/counter) com teto configurável. Espelha o Go.

THREAD_STATES = ("new", "negotiating", "settled", "rejected")
TERMINAL_STATES = ("settled", "rejected")

class Thread:
    """Uma negociação: request + os acts que o seguem (gramática mínima)."""
    def __init__(self, cap: int = 8):
        self.id = None            # id do request (raiz)
        self.target = None
        self.state = "new"
        self.last_offer = None    # id do último propose/counter
        self.offer_count = 0
        self.cap = cap if cap > 0 else 8
        self.trace: list[dict] = []

    def _trace(self, m: dict):
        self.trace.append({"act": m.get("act"), "id": m.get("id"),
                           "from": m.get("from", {}).get("id"),
                           "to": m.get("to", {}).get("id"),
                           "state": self.state})

    def apply(self, m: dict) -> None:
        """Transiciona a thread com uma mensagem. Levanta ValueError na
        primeira violação de gramática (a mensagem NÃO é aplicada)."""
        if m is None:
            raise ValueError("thread: mensagem nil")
        if self.state == "new":
            if m.get("act") != "request" or not m.get("target"):
                raise ValueError(f"thread: open exige 'request' com target "
                                 f"(obtido '{m.get('act')}' {m.get('id')})")
            self.id, self.target = m["id"], m["target"]
            self.state = "negotiating"
            self._trace(m)
            return
        if self.state in TERMINAL_STATES:
            raise ValueError(f"thread {self.id}: encerrada em {self.state} — "
                             f"ato '{m.get('act')}' ({m.get('id')}) inválido")
        # negotiating
        act = m.get("act")
        if act == "propose":
            self._offer(m)
        elif act == "counter":
            if self.last_offer is None:
                raise ValueError(f"thread: 'counter' exige oferta anterior "
                                 f"(thread {self.id}, msg {m.get('id')})")
            self._offer(m)
        elif act in ("accept", "commit"):
            self.state = "settled"
            self._trace(m)
        elif act in ("reject", "cancel"):
            self.state = "rejected"
            self._trace(m)
        elif act in ("delegate", "inform", "ask", "verify"):
            self._trace(m)  # lateral/delegação: sem mudança de estado
        else:
            raise ValueError(f"thread: ato '{act}' não reconhecido (msg {m.get('id')})")

    def _offer(self, m: dict):
        if self.offer_count + 1 > self.cap:
            raise ValueError(f"thread: teto de ofertas excedido ({self.cap}) — "
                             f"negociação sem final (thread {self.id}, msg {m.get('id')})")
        self.offer_count += 1
        self.last_offer = m["id"]
        self._trace(m)

def thread_from(cap: int, msgs: list[dict]) -> Thread:
    """Constrói a thread a partir das mensagens ordenadas (raiz primeiro).
    Levanta ValueError na primeira violação."""
    th = Thread(cap)
    for m in msgs:
        th.apply(m)
    return th

def validate(msg: dict, parent: dict | None = None, now: datetime | None = None) -> dict:
    """Retorna {schema_ok, signature_ok, sig_msg, tenant_ok, clauses: [...], warnings: [...]}."""
    from jsonschema import validate as jsval, ValidationError
    out = {"id": msg.get("id"), "act": msg.get("act"), "warnings": []}

    schema = json.load(open(__file__.replace("parley.py", "schema/parley-v0.schema.json")))
    try:
        jsval(msg, schema)
        out["schema_ok"] = True
    except ValidationError as e:
        out["schema_ok"] = False
        out["schema_error"] = e.message
        return out

    ok, why = verify(msg)
    out["signature_ok"], out["sig_msg"] = ok, why
    out["tenant_ok"] = bool(msg.get("tenant"))
    if not out["tenant_ok"]:
        out["warnings"].append("sem tenant (exigível em deploy multi-tenant)")

    now = now or datetime.now(timezone.utc)
    dl = msg.get("deadline")
    if dl and datetime.strptime(dl, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc) < now:
        out["warnings"].append(f"deadline expirado: {dl}")

    # contrato: cláusulas 'expect' do MENSAGEM-PAI avaliadas contra ESTA mensagem
    if parent and "expect" in parent and msg["act"] in ("propose", "counter", "accept", "commit"):
        accepted = set(msg.get("clauses", []))
        out["clauses"] = []
        for c in parent["expect"]:
            cid = c.get("id", "?")
            if "nl" in c:
                binding = cid in accepted
                out["clauses"].append({"id": cid, "nl": c["nl"], "binding": binding,
                                       "detail": "aceite explícito" if binding
                                       else "NÃO aceita explicitamente -> não vinculante"})
            else:
                okc, detail = check_clause(c, msg)
                out["clauses"].append({"id": cid, "check": c["check"], "ok": okc, "detail": detail})
    return out

# ---------------------------------------------------------------- CLI
def main():
    if len(sys.argv) < 2:
        print("uso: parley.py <arquivo.parley> [--sign] [--thread]")
        sys.exit(1)
    doc = parse_text(open(sys.argv[1]).read())
    msgs = finalize(doc)
    if "--sign" in sys.argv:
        msgs = [sign(m, m["from"]["id"], m.get("tenant", "default")) for m in msgs]
    if "--thread" in sys.argv and msgs:
        th = thread_from(8, msgs)
        print("\n--- THREAD ---")
        print(json.dumps({"id": th.id, "target": th.target, "state": th.state,
                          "offer_count": th.offer_count, "cap": th.cap,
                          "trace": th.trace}, indent=2, ensure_ascii=False))
    print(json.dumps({"capabilities": doc.capabilities, "messages": msgs},
                     indent=2, ensure_ascii=False))

if __name__ == "__main__":
    main()
