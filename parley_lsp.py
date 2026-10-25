#!/usr/bin/env python3
"""
PARLEY v0 — Language Server (LSP) for .parley files.

Zero third-party dependencies: JSON-RPC 2.0 over stdio with Content-Length
framing, using only the reference parser (parley.py) as the source of truth
for syntax + a tolerant analysis layer (shadow model) for multiple
diagnostics, completions, hover and symbols.

Capabilities:
  - textDocumentSync (full)  -> publishDiagnostics on every open/change
  - completionProvider       -> contextual keywords + agent ids,
                                capability names and input fields
  - hoverProvider            -> docs for keywords, agents, capabilities,
                                @msgN references
  - documentSymbolProvider   -> agents / capabilities / messages (tree)

Usage:
  python3 parley_lsp.py            # server (stdio)
  python3 parley_lsp.py --check F  # one-shot: diagnostics of F on stdout
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
# The canonical parser is parley.parse_text (fails on the 1st error). For an
# editor we want ALL the problems at once: hence a tolerant line-by-line scan
# that replicates the grammar (same regexes imported from parley)
# and accumulates diagnostics + declaration positions for symbols/hover.

TOP_KW = {"AGENT", "PROVIDES", "INPUT", "OUTPUT", "COST", "DEFAULT_CONFIDENCE"}
MSG_KW = ({a.upper() for a in ACTS} |
          {"INTENT", "WITH", "BY", "NOTE", "CONFIDENCE", "TENANT",
           "PARENT", "PROVENANCE", "FALLBACK", "EXPECT", "CLAUSES"})
CAP_NAME_RE = re.compile(r"^[a-z][a-z0-9_.]*\.[a-z][a-z0-9_.]*$")
TS_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(Z|[+-]\d{2}:\d{2})$")
MSGREF_RE = re.compile(r"^@msg(\d+)$")

KW_DOCS = {
    "AGENT": "Declares an agent and its local alias.\n`agent \"stable-id\" as ALIAS`",
    "PROVIDES": "Opens a capability declaration (input/output/cost contract).\n`provides \"route.plan\" v1`",
    "INPUT": "Input field of the capability being declared: `input name : Type`",
    "OUTPUT": "Output field of the capability being declared: `output name : Type`",
    "COST": "Cost constraint of the capability: `cost field op value` (e.g. `cost latency_ms <= 200`)",
    "DEFAULT_CONFIDENCE": "Default confidence of the capability (0..1).",
    "INTENT": "Natural-language intent for the receiver's LLM: `intent \"...\"`",
    "WITH": "Fills the structured body: `with k = v, k2 = [a, b]` (JSON values or literals).",
    "BY": "RFC3339 deadline: `by 2026-10-22T15:00:00Z`",
    "NOTE": "Short free-text note (e.g. a warning in a proposal).",
    "CONFIDENCE": "Sender's self-declared confidence in the body (0..1).",
    "TENANT": "Multi-tenant isolation key (enforceable in multi-tenant deployments).",
    "PARENT": "Id of the replied-to message: canonical id (`msg:hex`) or readable reference `@msgN`.",
    "PROVENANCE": "Chain of ids backing the message (audit): `provenance \"@msg1\", \"@msg2\"`.",
    "FALLBACK": "Contingency routing if the primary refuses or misses the deadline: `fallback -> \"agent-id\"`.",
    "EXPECT": "Contract clause. `check`: `expect field op value` (machine-validated). "
              "`nl`: `expect nl \"text\"` — only becomes BINDING if the counterparty accepts the clause "
              "explicitly in ACCEPT. Optional weight: `... w 0.9`.",
    "CLAUSES": "Parent's clause ids accepted explicitly: `clauses \"c1\", \"c3\"` "
               "(this is what makes 'nl' clauses binding).",
}
for _a in sorted(ACTS):
    KW_DOCS[_a.upper()] = {
        "inform": "Communicates a fact; no response required.",
        "ask": "Question; free answer.",
        "request": "Opens a negotiation: requires `target` (qualified capability).",
        "propose": "Offer against the `parent` (response to a request).",
        "counter": "Counter-offer; requires a previous offer in the thread.",
        "accept": "Closes the thread (settled). `clauses` makes 'nl' binding.",
        "reject": "Rejects; ends the thread (rejected).",
        "commit": "Confirms execution; ends the thread (settled).",
        "delegate": "Delegates to another agent; lateral (no thread state change).",
        "verify": "Requests verification; lateral.",
        "cancel": "Cancels; ends the thread (rejected).",
    }[_a]
for _op, _name in OPS.items():
    KW_DOCS[_op] = f"Clause `check` operator: `{_op}` ({_name})."


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
        """High-value semantic checks over the successfully parsed doc."""
        n = len(doc.messages)
        for i, m in enumerate(doc.messages, 1):
            line = messages[i - 1]["line"] if i - 1 < len(messages) else 0
            # @msgN out of range -> finalize() would raise IndexError
            for ref in [m.get("parent", "")] + list(m.get("provenance", [])):
                mm = MSGREF_RE.match(ref)
                if mm and not (1 <= int(mm.group(1)) <= n):
                    self.err(line, ref,
                             f"reference {ref!r} out of range ({n} messages exist)",
                             "semref", 1)
            fb = m.get("fallback", {})
            fb_id = fb.get("to", {}).get("id", "")
            if fb_id and fb_id not in {a["id"] for a in self.agents.values()}:
                self.err(line, fb_id,
                         f"fallback to undeclared agent: {fb_id!r}",
                         "unknown-fallback", 2)
            for c in m.get("expect", []):
                w = c.get("weight", 1.0)
                if not (0 < w <= 1):
                    self.err(line, str(w),
                             f"clause weight outside (0,1]: {w}", "clause-weight", 2)
            if m.get("act") == "request" and not m.get("target"):
                self.err(line, "request",
                         "request without target (the thread requires a qualified capability)",
                         "no-target", 2)
            for c in m.get("expect", []):
                chk = c.get("check", {})
                fld = chk.get("field", "")
                if chk and fld and "." not in fld and fld != "confidence":
                    # body path: weak heuristic, informational only
                    pass
            # accepted clauses must exist in the parent
            cl = m.get("clauses")
            if cl and m.get("parent"):
                pm = MSGREF_RE.match(m["parent"])
                if pm and 1 <= int(pm.group(1)) <= n:
                    parent = doc.messages[int(pm.group(1)) - 1]
                    valid = {c.get("id") for c in parent.get("expect", [])}
                    for cid in cl:
                        if cid not in valid:
                            self.err(line, cid,
                                     f"clause {cid!r} does not exist in the parent message's expect",
                                     "unknown-clause", 2)


def _line_of(src_lines, i):
    return src_lines[i] if i < len(src_lines) else ""


def analyze(src: str) -> Model:
    """Tolerant scan + canonical parser. Never raises an exception."""
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
                model.err(i, stripped, "malformed AGENT (expected: agent \"id\" as ALIAS)",
                          "bad-agent")
                cur_agent = None
            else:
                cur_agent, cur_cap, cur_msg = m.group(2), None, None
                model.agents[cur_agent] = {"id": m.group(1), "line": i, "provides": []}
            continue

        if first == "PROVIDES":
            m = re.match(r'^PROVIDES\s+"?([a-z0-9_.]+)"?\s+V(\d+)$', stripped, re.I)
            if not m:
                model.err(i, stripped, "malformed PROVIDES (expected: provides \"name\" vN)")
                cur_cap = None
            elif cur_agent is None:
                model.err(i, stripped, "PROVIDES before AGENT", "order")
                cur_cap = None
            else:
                cur_cap, cur_msg = m.group(1), None
                model.caps.setdefault(cur_cap, {"line": i, "version": int(m.group(2)),
                                                "by": cur_agent})
                if cur_cap not in model.agents[cur_agent]["provides"]:
                    model.agents[cur_agent]["provides"].append(cur_cap)
                if not CAP_NAME_RE.match(cur_cap):
                    model.err(i, cur_cap,
                              f"capability name {cur_cap!r} does not follow the protocol's "
                              f"qualified `x.y` pattern", "cap-name", 2)
            continue

        if first in ("INPUT", "OUTPUT", "COST", "DEFAULT_CONFIDENCE"):
            if cur_cap is None:
                model.err(i, stripped, f"{first} outside a capability declaration",
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
                      f"header references undeclared agent(s): {', '.join(missing)}",
                      "unknown-agent")
            continue

        if cur_msg is None and first not in TOP_KW:
            model.err(i, stripped, "line out of context (agent/capability/message)",
                      "orphan")
            continue

        if cur_msg:
            if first not in MSG_KW:
                model.err(i, stripped, f"unknown token in message: {first!r}",
                          "unknown-token")
            elif first == "EXPECT":
                rest = stripped[len("EXPECT"):].strip()
                wm = re.search(r"\s+W\s+(\d+(\.\d+)?)\s*$", rest, re.I)
                if wm:
                    try:
                        w = float(wm.group(1))
                        if not (0 < w <= 1):
                            model.err(i, wm.group(1),
                                      f"clause weight outside (0,1]: {w}", "clause-weight", 2)
                    except ValueError:
                        model.err(i, wm.group(1), f"invalid weight: {wm.group(1)}",
                                  "clause-weight", 2)
            elif first == "BY":
                val = stripped.split(None, 1)[1].strip() if " " in stripped else ""
                if not TS_RE.match(val):
                    model.err(i, val, f"deadline is not RFC3339: {val!r}", "bad-deadline", 2)
        continue

    # canonical parser: source of truth + semantics
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
    except Exception as e:  # IndexError from @msgN etc.
        model.parse_ok = False
        model.parse_error = str(e)
        model.diags.append(Diag(0, 0, 0, f"parser: {e}", "parse", 1))

    # dedupe diagnostics at the same position/message
    seen, out = set(), []
    for d in sorted(model.diags, key=lambda d: (d.line, d.col0)):
        k = (d.line, d.col0, d.msg)
        if k not in seen:
            seen.add(k)
            out.append(d)
    model.diags = out
    return model


def context_at(model: Model, line_idx: int) -> dict:
    """Line context: top | capability | message (for completion).

    Rule: the last BLOCK opened before the line (agent/provides/message,
    in temporal order) determines the context.
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
            else:  # agent: keeps the previous neutral block (top)
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
        if method is None:  # response to something we did not send; ignore
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
                                  "message": f"unsupported method: {method}"}}
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

        # special prefixes: complete values, not keywords
        mfb = re.match(r'^(.*FALLBACK\s*->\s*)$', prefix, re.I)
        if mfb:
            for a in model.agents.values():
                add(f'"{a["id"]}"', 2, "agent id")  # 2 = Value
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
                                  "value": f"**agent `{word}`**\n\n"
                                           f"- id: `{a['id']}`\n- provides: {prov}"}}
        if word in model.caps:
            c = model.caps[word]
            return {"contents": {"kind": "markdown",
                                  "value": f"**capability `{word}`**\n\n"
                                           f"- version: v{c['version']}\n"
                                           f"- declared by: `{c['by']}`"}}
        mref = MSGREF_RE.match(word)
        if mref and 1 <= int(mref.group(1)) <= len(model.messages):
            mi = model.messages[int(mref.group(1)) - 1]
            return {"contents": {"kind": "markdown",
                                  "value": f"**`{word}`** — message #"
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
                "detail": "message",
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
            sys.stderr.write(f"parley-lsp: invalid framing: {e}\n")
            continue
        if msg is None:
            return
        try:
            result = srv._handle(msg)
        except SystemExit as e:
            sys.stdout.buffer.flush()
            raise e
        except Exception as e:  # never kills the server over 1 document
            sys.stderr.write(f"parley-lsp: error in {msg.get('method')}: {e}\n")
            if msg.get("id") is not None:
                result = {"code": -32603, "message": f"internal error: {e}"}
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