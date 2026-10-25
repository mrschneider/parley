#!/usr/bin/env python3
"""
PARLEY v0 — Language Server (LSP) para arquivos .parley.

Zero dependências de terceiros: JSON-RPC 2.0 sobre stdio com framing
Content-Length, usando apenas o parser de referência (parley.py) como
fonte da verdade de sintaxe + uma camada de análise tolerante (shadow
model) para múltiplos diagnostics, completions, hover e símbolos.

Capacidades:
  - textDocumentSync (full)  -> publishDiagnostics a cada open/change
  - completionProvider       -> keywords contextuais + ids de agentes,
                                nomes de capabilities e campos de input
  - hoverProvider            -> docs de keywords, agentes, capabilities,
                                referências @msgN
  - documentSymbolProvider   -> agentes / capabilities / mensagens (árvore)

Uso:
  python3 parley_lsp.py            # server (stdio)
  python3 parley_lsp.py --check F  # one-shot: diagnostics de F no stdout
"""
from __future__ import annotations
import json
import re
import sys
from dataclasses import dataclass, field

from parley import ACTS, OPS, HEADER_RE, parse_text, parse_value

SERVER_INFO = {"name": "parley-lsp", "version": "0.1.0"}

# ---------------------------------------------------------------- shadow model
#
# O parser canônico é parley.parse_text (falha no 1º erro). Para um editor
# queremos TODOS os problemas de uma vez: por isso uma varredura tolerante
# linha a linha que replica a gramática (mesmos regex importados de parley)
# e acumula diagnostics + posições de declaração para symbols/hover.

TOP_KW = {"AGENT", "PROVIDES", "INPUT", "OUTPUT", "COST", "DEFAULT_CONFIDENCE"}
MSG_KW = ({a.upper() for a in ACTS} |
          {"INTENT", "WITH", "BY", "NOTE", "CONFIDENCE", "TENANT",
           "PARENT", "PROVENANCE", "FALLBACK", "EXPECT", "CLAUSES"})
CAP_NAME_RE = re.compile(r"^[a-z][a-z0-9_.]*\.[a-z][a-z0-9_.]*$")
TS_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})$")
MSGREF_RE = re.compile(r"^@msg(\d+)$")

KW_DOCS = {
    "AGENT": "Declara um agente e seu alias local.\n`agent \"id-estavel\" as ALIAS`",
    "PROVIDES": "Abre a declaração de uma capability (contrato de input/output/custo).\n`provides \"route.plan\" v1`",
    "INPUT": "Campo de input da capability em declaração: `input nome : Tipo`",
    "OUTPUT": "Campo de output da capability em declaração: `output nome : Tipo`",
    "COST": "Restrição de custo da capability: `cost campo op valor` (ex.: `cost latency_ms <= 200`)",
    "DEFAULT_CONFIDENCE": "Confiança padrão da capability (0..1).",
    "INTENT": "Intenção em linguagem natural para o LLM do recebedor: `intent \"...\"`",
    "WITH": "Preenche o body estruturado: `with k = v, k2 = [a, b]` (valores JSON ou literais).",
    "BY": "Deadline RFC3339: `by 2026-10-22T15:00:00Z`",
    "NOTE": "Nota curta em texto livre (ex.: aviso numa proposta).",
    "CONFIDENCE": "Confiança auto-declarada do remetente no body (0..1).",
    "TENANT": "Chave de isolamento multi-tenant (exigível em deploy multi-tenant).",
    "PARENT": "Id da mensagem respondida: id canônico (`msg:hex`) ou referência legível `@msgN`.",
    "PROVENANCE": "Cadeia de ids que fundamentam a mensagem (auditoria): `provenance \"@msg1\", \"@msg2\"`.",
    "FALLBACK": "Roteamento de contingência se o primário recusar ou estourar o deadline: `fallback -> \"agent-id\"`.",
    "EXPECT": "Cláusula de contrato. `check`: `expect campo op valor` (validada por máquina). "
              "`nl`: `expect nl \"texto\"` — só fica VINCULANTE se o contraparte aceitar a cláusula "
              "explicitamente em ACCEPT. Peso opcional: `... w 0.9`.",
    "CLAUSES": "Ids de cláusulas do pai aceitos explicitamente: `clauses \"c1\", \"c3\"` "
               "(é isso que torna cláusulas 'nl' vinculantes).",
}
for _a in sorted(ACTS):
    KW_DOCS[_a.upper()] = {
        "inform": "Comunica um fato; não exige resposta.",
        "ask": "Pergunta; resposta livre.",
        "request": "Abre uma negociação: exige `target` (capability qualificada).",
        "propose": "Oferta contra o `parent` (resposta a um request).",
        "counter": "Contra-oferta; exige oferta anterior na thread.",
        "accept": "Fecha a thread (settled). `clauses` torna 'nl' vinculante.",
        "reject": "Rejeita; encerra a thread (rejected).",
        "commit": "Confirma execução; encerra a thread (settled).",
        "delegate": "Delega a outro agente; lateral (não muda estado da thread).",
        "verify": "Solicita verificação; lateral.",
        "cancel": "Cancela; encerra a thread (rejected).",
    }[_a]
for _op, _name in OPS.items():
    KW_DOCS[_op] = f"Operador de cláusula `check`: `{_op}` ({_name})."


@dataclass
class Diag:
    line: int          # 0-based (LSP)
    col0: int
    col1: int
    msg: str
    severity: int = 1  # 1=Error 2=Warning 3=Info 4=Hint
    code: str = ""

    def to_lsp(self):
        return {
            "range": {"start": {"line": self.line, "character": self.col0},
                      "end": {"line": self.line, "character": self.col1}},
            "severity": self.severity,
            "source": "parley",
            "code": self.code,
            "message": self.msg,
        }


@dataclass
class Model:
    diags: list[Diag] = field(default_factory=list)
    agents: dict[str, dict] = field(default_factory=dict)   # alias -> {id, line, provides:[name]}
    caps: dict[str, dict] = field(default_factory=dict)     # name -> {line, version, by}
    messages: list[dict] = field(default_factory=list)      # {line, frm, to}
    parse_ok: bool = False
    parse_error: str | None = None

    def err(self, ln0, s, msg, code="syntax", sev=1):
        self.diags.append(Diag(ln0, max(0, len(s) - len(s.lstrip())), len(s.lstrip()),
                               msg, sev, code))

    def semantic(self, doc, messages):
        """Check semânticos de alto valor sobre o doc parseado com sucesso."""
        n = len(doc.messages)
        for i, m in enumerate(doc.messages, 1):
            line = messages[i - 1]["line"] if i - 1 < len(messages) else 0
            # @msgN fora do alcance -> finalize() estouraria IndexError
            for ref in [m.get("parent", "")] + list(m.get("provenance", [])):
                mm = MSGREF_RE.match(ref)
                if mm and not (1 <= int(mm.group(1)) <= n):
                    self.err(line, ref,
                             f"referência {ref!r} fora do alcance (existem {n} mensagens)",
                             "semref", 1)
            fb = m.get("fallback", {})
            fb_id = fb.get("to", {}).get("id", "")
            if fb_id and fb_id not in {a["id"] for a in self.agents.values()}:
                self.err(line, fb_id,
                         f"fallback para agente não declarado: {fb_id!r}",
                         "unknown-fallback", 2)
            for c in m.get("expect", []):
                w = c.get("weight", 1.0)
                if not (0 < w <= 1):
                    self.err(line, str(w),
                             f"peso de cláusula fora de (0,1]: {w}", "clause-weight", 2)
            if m.get("act") == "request" and not m.get("target"):
                self.err(line, "request",
                         "request sem target (a thread exige capability qualificada)",
                         "no-target", 2)
            for c in m.get("expect", []):
                chk = c.get("check", {})
                fld = chk.get("field", "")
                if chk and fld and "." not in fld and fld != "confidence":
                    # caminho de corpo: heurística fraca, apenas informativo
                    pass
            # clauses aceitas devem existir no parent
            cl = m.get("clauses")
            if cl and m.get("parent"):
                pm = MSGREF_RE.match(m["parent"])
                if pm and 1 <= int(pm.group(1)) <= n:
                    parent = doc.messages[int(pm.group(1)) - 1]
                    valid = {c.get("id") for c in parent.get("expect", [])}
                    for cid in cl:
                        if cid not in valid:
                            self.err(line, cid,
                                     f"cláusula {cid!r} não existe no expect da mensagem pai",
                                     "unknown-clause", 2)


def _line_of(src_lines, i):
    return src_lines[i] if i < len(src_lines) else ""


def analyze(src: str) -> Model:
    """Varredura tolerante + parser canônico. Nunca levanta exceção."""
    model = Model()
    lines = src.splitlines()
    cur_agent, cur_cap, cur_msg = None, None, None
    msg_idx = 0

    for i, raw in enumerate(lines):
        stripped = raw.strip()
        if not stripped or stripped.startswith("#"):
            continue
        first = stripped.split(None, 1)[0].upper()

        if first == "AGENT":
            m = re.match(r'^AGENT\s+"([^"]+)"\s+AS\s+([A-Za-z_][\w-]*)$', stripped, re.I)
            if not m:
                model.err(i, stripped, "AGENT malformado (esperado: agent \"id\" as ALIAS)",
                          "bad-agent")
                cur_agent = None
            else:
                cur_agent, cur_cap, cur_msg = m.group(2), None, None
                model.agents[cur_agent] = {"id": m.group(1), "line": i, "provides": []}
            continue

        if first == "PROVIDES":
            m = re.match(r'^PROVIDES\s+"?([a-z0-9_.]+)"?\s+V(\d+)$', stripped, re.I)
            if not m:
                model.err(i, stripped, "PROVIDES malformado (esperado: provides \"nome\" vN)")
                cur_cap = None
            elif cur_agent is None:
                model.err(i, stripped, "PROVIDES antes de AGENT", "order")
                cur_cap = None
            else:
                cur_cap, cur_msg = m.group(1), None
                model.caps.setdefault(cur_cap, {"line": i, "version": int(m.group(2)),
                                                "by": cur_agent})
                if cur_cap not in model.agents[cur_agent]["provides"]:
                    model.agents[cur_agent]["provides"].append(cur_cap)
                if not CAP_NAME_RE.match(cur_cap):
                    model.err(i, cur_cap,
                              f"nome de capability {cur_cap!r} não segue o padrão "
                              f"qualificado `x.y` do protocolo", "cap-name", 2)
            continue

        if first in ("INPUT", "OUTPUT", "COST", "DEFAULT_CONFIDENCE"):
            if cur_cap is None:
                model.err(i, stripped, f"{first} fora de uma declaração de capability",
                          "order")
            continue

        hm = HEADER_RE.match(stripped)
        if hm and hm.group(1) in model.agents and hm.group(2) in model.agents:
            cur_agent, cur_cap = None, None
            cur_msg = True
            msg_idx += 1
            model.messages.append({"line": i, "frm": hm.group(1), "to": hm.group(2)})
            continue
        if hm and (hm.group(1) not in model.agents or hm.group(2) not in model.agents):
            missing = [g for g in (hm.group(1), hm.group(2)) if g not in model.agents]
            model.err(i, stripped,
                      f"header referencia agente(s) não declarado(s): {', '.join(missing)}",
                      "unknown-agent")
            continue

        if cur_msg is None and first not in TOP_KW:
            model.err(i, stripped, "linha fora de contexto (agente/capacidade/mensagem)",
                      "orphan")
            continue

        if cur_msg:
            if first not in MSG_KW:
                model.err(i, stripped, f"token desconhecido em mensagem: {first!r}",
                          "unknown-token")
            elif first == "EXPECT":
                rest = stripped[len("EXPECT"):].strip()
                wm = re.search(r"\s+W\s+(\d+(\.\d+)?)\s*$", rest, re.I)
                if wm:
                    try:
                        w = float(wm.group(1))
                        if not (0 < w <= 1):
                            model.err(i, wm.group(1),
                                      f"peso de cláusula fora de (0,1]: {w}", "clause-weight", 2)
                    except ValueError:
                        model.err(i, wm.group(1), f"peso inválido: {wm.group(1)}",
                                  "clause-weight", 2)
            elif first == "BY":
                val = stripped.split(None, 1)[1].strip() if " " in stripped else ""
                if not TS_RE.match(val):
                    model.err(i, val, f"deadline não é RFC3339: {val!r}", "bad-deadline", 2)
        continue

    # parser canônico: fonte da verdade + semântica
    try:
        doc = parse_text(src)
        model.parse_ok = True
        model.semantic(doc, model.messages)
    except SyntaxError as e:
        model.parse_ok = False
        model.parse_error = str(e)
        m = re.match(r"ln (\d+):", str(e))
        if m:
            ln = int(m.group(1)) - 1
            model.diags.append(Diag(ln, 0, len(_line_of(lines, ln)),
                                    f"parser: {e}", "parse", 1))
        else:
            model.diags.append(Diag(0, 0, 0, f"parser: {e}", "parse", 1))
    except Exception as e:  # IndexError de @msgN etc.
        model.parse_ok = False
        model.parse_error = str(e)
        model.diags.append(Diag(0, 0, 0, f"parser: {e}", "parse", 1))

    # desduplica diagnostics na mesma posição/mensagem
    seen, out = set(), []
    for d in sorted(model.diags, key=lambda d: (d.line, d.col0)):
        k = (d.line, d.col0, d.msg)
        if k not in seen:
            seen.add(k)
            out.append(d)
    model.diags = out
    return model


def context_at(model: Model, line_idx: int) -> dict:
    """Contexto da linha: top | capability | message (para completion).

    Regra: o último BLOCO aberto antes da linha (agent/provides/message,
    em ordem temporal) determina o contexto.
    """
    blocks = []
    for name, c in model.caps.items():
        blocks.append((c["line"], "capability", name))
    for m in model.messages:
        blocks.append((m["line"], "message", None))
    for a in model.agents.values():
        blocks.append((a["line"], "agent", None))
    blocks.sort()
    ctx = {"kind": "top", "cap": None}
    for line, kind, name in blocks:
        if line <= line_idx:
            if kind == "capability":
                ctx = {"kind": "capability", "cap": name}
            elif kind == "message":
                ctx = {"kind": "message"}
            else:  # agent: mantém bloco anterior neutro (top)
                ctx = {"kind": "top", "cap": None}
    return ctx


# ---------------------------------------------------------------- LSP plumbing

def _encode(obj) -> bytes:
    body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
    return b"Content-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body


def read_message(f) -> dict | None:
    headers = {}
    while True:
        line = f.readline()
        if not line:
            return None
        line = line.decode("ascii", "replace").strip()
        if line == "":
            break
        k, _, v = line.partition(":")
        headers[k.strip().lower()] = v.strip()
    length = int(headers.get("content-length", "0"))
    if length <= 0:
        return None
    return json.loads(f.read(length).decode("utf-8"))


def uri_path(uri: str) -> str:
    if uri.startswith("file://"):
        from urllib.parse import unquote, urlparse
        return unquote(urlparse(uri).path)
    return uri


class Server:
    def __init__(self):
        self.docs: dict[str, str] = {}
        self.models: dict[str, Model] = {}
        self.shutdown = False

    # ---- handlers -----------------------------------------------------------
    def _diagnostics(self, uri: str):
        model = self.models.get(uri)
        return [d.to_lsp() for d in model.diags] if model else []

    def _publish_diagnostics(self, uri: str):
        self._send({"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics",
                    "params": {"uri": uri, "diagnostics": self._diagnostics(uri)}})

    def _handle(self, msg: dict):
        method, mid, params = msg.get("method"), msg.get("id"), msg.get("params") or {}
        if method is None:  # resposta a algo que não enviamos; ignora
            return
        if method == "initialize":
            return {
                "capabilities": {
                    "textDocumentSync": 1,  # full
                    "completionProvider": {"triggerCharacters": [" ", ">", ".", "-"],
                                           "resolveProvider": False},
                    "hoverProvider": True,
                    "documentSymbolProvider": True,
                },
                "serverInfo": SERVER_INFO,
            }
        if method == "initialized":
            return None
        if method in ("textDocument/didOpen", "textDocument/didChange"):
            uri = params["textDocument"]["uri"]
            if method == "textDocument/didOpen":
                self.docs[uri] = params["textDocument"]["text"]
            else:
                self.docs[uri] = params["contentChanges"][-1]["text"]
            self.models[uri] = analyze(self.docs[uri])
            self._publish_diagnostics(uri)
            return None
        if method == "textDocument/didClose":
            uri = params["textDocument"]["uri"]
            self.docs.pop(uri, None)
            self.models.pop(uri, None)
            return None
        if method == "textDocument/completion":
            return {"isIncomplete": False,
                    "items": self._complete(params)}
        if method == "textDocument/hover":
            h = self._hover(params)
            return h
        if method == "textDocument/documentSymbol":
            return self._symbols(params["textDocument"]["uri"])
        if method == "shutdown":
            self.shutdown = True
            return None
        if method == "exit":
            raise SystemExit(0 if self.shutdown else 1)
        if mid is not None:
            return {"__error__": {"code": -32601,
                                  "message": f"método não suportado: {method}"}}
        return None

    def _send(self, obj):
        sys.stdout.buffer.write(_encode(obj))
        sys.stdout.buffer.flush()

    # ---- completion ---------------------------------------------------------
    def _complete(self, params):
        uri = params["textDocument"]["uri"]
        text = self.docs.get(uri, "")
        pos = params["position"]
        lines = text.splitlines()
        line = lines[pos["line"]] if pos["line"] < len(lines) else ""
        prefix = line[:pos["character"]]
        model = self.models.get(uri)
        if model is None:
            model = analyze(text)
        ctx = context_at(model, pos["line"])
        items = []

        def add(label, kind=14, detail=""):  # 14 = Keyword
            items.append({"label": label, "kind": kind, "detail": detail,
                          "filterText": label})

        # prefixos especiais: completar valores, não keywords
        mfb = re.match(r'^(.*FALLBACK\s*->\s*)$', prefix, re.I)
        if mfb:
            for a in model.agents.values():
                add(f'"{a["id"]}"', 2, "id de agente")  # 2 = Value
            return items
        mact = re.match(r'^\s*([A-Za-z_]+\s+)$', prefix)
        if mact and mact.group(1).split()[0].lower() in {"request", "propose",
                                                         "accept", "counter"}:
            for name in model.caps:
                add(name, 6, "capability")  # 6 = Class
            return items
        if prefix.strip() == "":
            if ctx["kind"] == "message":
                for k in sorted(MSG_KW):
                    add(k, 14, KW_DOCS.get(k, "").splitlines()[0][:60])
            elif ctx["kind"] == "capability":
                for k in ("INPUT", "OUTPUT", "COST", "DEFAULT_CONFIDENCE"):
                    add(k, 14, KW_DOCS[k].splitlines()[0][:60])
            else:
                for k in ("AGENT", "PROVIDES"):
                    add(k, 14, KW_DOCS[k].splitlines()[0][:60])
        else:
            word = prefix.split()[-1].lstrip('"').upper() if prefix.split() else ""
            pool = MSG_KW if ctx["kind"] == "message" else \
                (TOP_KW | MSG_KW)
            for k in sorted(pool):
                if word and k.startswith(word) and k != word:
                    add(k, 14, KW_DOCS.get(k, "").splitlines()[0][:60])
        return items

    # ---- hover ---------------------------------------------------------------
    def _hover(self, params):
        uri = params["textDocument"]["uri"]
        text = self.docs.get(uri, "")
        pos = params["position"]
        lines = text.splitlines()
        line = lines[pos["line"]] if pos["line"] < len(lines) else ""
        col = pos["character"]
        word = None
        for mm in re.finditer(r'[\w.@:.-]+', line):
            if mm.start() <= col <= mm.end():
                word = mm.group(0)
                break
        if word is None:
            return None
        model = self.models.get(uri) or analyze(text)
        md = word.upper()
        if md in KW_DOCS:
            return {"contents": {"kind": "markdown",
                                  "value": f"**`{word}`** — {KW_DOCS[md]}"}}
        if word in model.agents:
            a = model.agents[word]
            prov = ", ".join(a["provides"]) or "—"
            return {"contents": {"kind": "markdown",
                                  "value": f"**agente `{word}`**\n\n"
                                           f"- id: `{a['id']}`\n- provides: {prov}"}}
        if word in model.caps:
            c = model.caps[word]
            return {"contents": {"kind": "markdown",
                                  "value": f"**capability `{word}`**\n\n"
                                           f"- versão: v{c['version']}\n"
                                           f"- declarada por: `{c['by']}`"}}
        mref = MSGREF_RE.match(word)
        if mref and 1 <= int(mref.group(1)) <= len(model.messages):
            mi = model.messages[int(mref.group(1)) - 1]
            return {"contents": {"kind": "markdown",
                                  "value": f"**`{word}`** — mensagem nº "
                                           f"{mref.group(1)}: `{mi['frm']} → {mi['to']}`"}}
        return None

    # ---- documentSymbol -------------------------------------------------------
    def _symbols(self, uri):
        model = self.models.get(uri) or analyze(self.docs.get(uri, ""))

        def rng(line):
            return {"start": {"line": line, "character": 0},
                    "end": {"line": line, "character": 0}}

        out = []
        for alias, a in model.agents.items():
            out.append({
                "name": alias, "detail": a["id"], "kind": 3,  # Namespace
                "range": rng(a["line"]), "selectionRange": rng(a["line"]),
                "children": [{"name": p, "kind": 6, "range": rng(a["line"]),
                              "selectionRange": rng(a["line"])}
                             for p in a["provides"]],
            })
        for name, c in model.caps.items():
            out.append({
                "name": name, "detail": f"v{c['version']} (by {c['by']})",
                "kind": 6,  # Class
                "range": rng(c["line"]), "selectionRange": rng(c["line"]),
            })
        for i, m in enumerate(model.messages, 1):
            out.append({
                "name": f"msg{i}: {m['frm']} → {m['to']}",
                "detail": "mensagem",
                "kind": 10,  # Method
                "range": rng(m["line"]), "selectionRange": rng(m["line"]),
            })
        return out


# ---------------------------------------------------------------- loop

def serve():
    srv = Server()
    inp = sys.stdin.buffer
    while True:
        try:
            msg = read_message(inp)
        except (json.JSONDecodeError, ValueError) as e:
            sys.stderr.write(f"parley-lsp: framing inválido: {e}\n")
            continue
        if msg is None:
            return
        try:
            result = srv._handle(msg)
        except SystemExit as e:
            sys.stdout.buffer.flush()
            raise e
        except Exception as e:  # nunca derruba o server por 1 documento
            sys.stderr.write(f"parley-lsp: erro em {msg.get('method')}: {e}\n")
            if msg.get("id") is not None:
                result = {"code": -32603, "message": f"erro interno: {e}"}
        if msg.get("id") is not None and result is not None:
            if "__error__" in result:
                srv._send({"jsonrpc": "2.0", "id": msg["id"],
                           "error": result["__error__"]})
            else:
                srv._send({"jsonrpc": "2.0", "id": msg["id"], "result": result})
        elif msg.get("id") is not None:
            srv._send({"jsonrpc": "2.0", "id": msg["id"], "result": None})


def check_file(path: str):
    model = analyze(open(path).read())
    diags = [d.to_lsp() for d in model.diags]
    print(json.dumps({"file": path, "parse_ok": model.parse_ok,
                      "diagnostics": diags}, indent=2, ensure_ascii=False))
    return 1 if any(d["severity"] == 1 for d in diags) else 0


if __name__ == "__main__":
    if len(sys.argv) >= 3 and sys.argv[1] == "--check":
        sys.exit(check_file(sys.argv[2]))
    serve()
