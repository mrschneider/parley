#!/usr/bin/env python3
"""PARLEY v0 — testes do LSP (cliente JSON-RPC real sobre stdio).

Sobe `python3 parley_lsp.py` como subprocess e valida:
initialize, publishDiagnostics (open/change/close), completion (keywords,
capabilities, agentes), hover (keyword/agente/@msgN) e documentSymbol.

Rode: python3 -m pytest test_lsp.py -q   (ou: python3 test_lsp.py)
"""
from __future__ import annotations
import json
import os
import subprocess
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

HERE = os.path.dirname(os.path.abspath(__file__))
EXAMPLE = open(os.path.join(HERE, "examples", "negociacao.parley")).read()

URI = "file:///tmp/negociacao.parley"


class LspClient:
    """Cliente LSP mínimo: 1 thread de leitura, writes com framing."""

    def __init__(self):
        self.proc = subprocess.Popen(
            [sys.executable, os.path.join(HERE, "parley_lsp.py")],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, cwd=HERE)
        self._id = 0
        self._responses = {}      # id -> result|error
        self._events = []         # (method, params)
        self._lock = threading.Lock()
        self._t = threading.Thread(target=self._reader, daemon=True)
        self._t.start()

    def _reader(self):
        f = self.proc.stdout
        while True:
            headers = {}
            line = f.readline()
            if not line:
                return
            line = line.decode().strip()
            if line:
                k, _, v = line.partition(":")
                headers[k.strip().lower()] = v.strip()
            while True:  # cabeçalhos até linha vazia
                l2 = f.readline()
                if not l2:
                    return
                if l2.decode().strip() == "":
                    break
                k, _, v = l2.decode().strip().partition(":")
                headers[k.strip().lower()] = v.strip()
            body = f.read(int(headers.get("content-length", "0")))
            msg = json.loads(body.decode())
            with self._lock:
                if "id" in msg and ("result" in msg or "error" in msg):
                    self._responses[msg["id"]] = msg
                elif "method" in msg:
                    self._events.append((msg["method"], msg.get("params", {})))

    def _send(self, obj):
        body = json.dumps(obj).encode()
        self.proc.stdin.write(b"Content-Length: %d\r\n\r\n%s" % (len(body), body))
        self.proc.stdin.flush()

    def notify(self, method, params):
        self._send({"jsonrpc": "2.0", "method": method, "params": params})

    def request(self, method, params, timeout=5.0):
        with self._lock:
            self._id += 1
            mid = self._id
        self._send({"jsonrpc": "2.0", "id": mid, "method": method, "params": params})
        t0 = time.time()
        while time.time() - t0 < timeout:
            with self._lock:
                if mid in self._responses:
                    return self._responses.pop(mid)
            time.sleep(0.01)
        raise TimeoutError(f"sem resposta para {method}")

    def wait_event(self, method, timeout=5.0):
        t0 = time.time()
        while time.time() - t0 < timeout:
            with self._lock:
                for i, (m, p) in enumerate(self._events):
                    if m == method:
                        del self._events[i]
                        return p
            time.sleep(0.01)
        raise TimeoutError(f"sem evento {method}")

    def diags(self):
        p = self.wait_event("textDocument/publishDiagnostics")
        assert p["uri"] == URI
        return p["diagnostics"]

    def stop(self):
        try:
            self.notify("exit", {})
        except Exception:
            pass
        self.proc.terminate()
        try:
            self.proc.wait(timeout=3)
        except Exception:
            self.proc.kill()


def test_initialize_capabilities():
    c = LspClient()
    try:
        r = c.request("initialize", {"processId": None, "rootUri": None,
                                     "capabilities": {}})
        caps = r["result"]["capabilities"]
        assert caps["textDocumentSync"] == 1
        assert caps["hoverProvider"] is True
        assert caps["documentSymbolProvider"] is True
        assert caps["completionProvider"]["triggerCharacters"]
        assert r["result"]["serverInfo"]["name"] == "parley-lsp"
    finally:
        c.stop()


def test_diagnostics_clean_example_then_broken():
    c = LspClient()
    try:
        c.notify("initialized", {})
        c.notify("textDocument/didOpen", {"textDocument": {"uri": URI,
                "languageId": "parley", "version": 1, "text": EXAMPLE}})
        assert c.diags() == [], "exemplo limpo não deve ter diagnostics"

        # muda para documento quebrado: agente não declarado + linha órfã
        broken = (
            'agent "planner-42" as PLANNER\n'
            'PLANNER -> GHOST\n'
            '  request route.plan\n'
            'frobloque "x"\n'
        )
        c.notify("textDocument/didChange", {"textDocument": {"uri": URI,
                "version": 2}, "contentChanges": [{"text": broken}]})
        d = c.diags()
        codes = {x["code"] for x in d}
        assert "unknown-agent" in codes, codes
        assert "orphan" in codes, codes
        assert all(x["source"] == "parley" for x in d)

        # fecha: server limpa o estado (sem crash) e reabre limpo
        c.notify("textDocument/didClose", {"textDocument": {"uri": URI}})
        c.notify("textDocument/didOpen", {"textDocument": {"uri": URI,
                "languageId": "parley", "version": 3, "text": EXAMPLE}})
        assert c.diags() == []
    finally:
        c.stop()


def test_semantic_diagnostics_msgref_and_weight():
    c = LspClient()
    try:
        bad = (
            'agent "a" as A\n'
            'agent "b" as B\n'
            'A -> B\n'
            '  request x.y\n'
            '  expect confidence > 0.5 w 2.5\n'
            'B -> A\n'
            '  propose x.y\n'
            '  parent "@msg9"\n'
        )
        c.notify("textDocument/didOpen", {"textDocument": {"uri": URI,
                "languageId": "parley", "version": 1, "text": bad}})
        d = c.diags()
        codes = {x["code"] for x in d}
        assert "semref" in codes, codes       # @msg9 fora do alcance
        assert "clause-weight" in codes, codes  # peso 2.5 fora de (0,1]
    finally:
        c.stop()


def test_completion_top_message_capability():
    c = LspClient()
    try:
        c.notify("textDocument/didOpen", {"textDocument": {"uri": URI,
                "languageId": "parley", "version": 1, "text": EXAMPLE}})
        c.wait_event("textDocument/publishDiagnostics")

        def labels(pos):
            r = c.request("textDocument/completion", {
                "textDocument": {"uri": URI}, "position": pos})
            return {i["label"] for i in r["result"]["items"]}

        lines = EXAMPLE.splitlines()
        prov_line = next(i for i, l in enumerate(lines) if l.strip().startswith("provides"))
        in_line = next(i for i, l in enumerate(lines) if l.strip().startswith("input"))

        # contexto de capability (linha de 'input origin : Location')
        top = labels({"line": in_line, "character": 0})
        assert "INPUT" in top and "COST" in top, top

        # contexto de mensagem (linha do ato 'request route.plan')
        req_line = next(i for i, l in enumerate(lines)
                        if l.strip() == "request route.plan")
        msg_kw = labels({"line": req_line, "character": 0})
        assert "EXPECT" in msg_kw and "WITH" in msg_kw and "INTENT" in msg_kw, msg_kw

        # completar fallback: ids de agentes
        fb_line = next(i for i, l in enumerate(lines)
                       if l.strip().startswith("fallback ->"))
        r = c.request("textDocument/completion", {
            "textDocument": {"uri": URI},
            "position": {"line": fb_line, "character": len("  fallback -> ")}})
        fbs = {i["label"] for i in r["result"]["items"]}
        assert '"dispatcher-backup"' in fbs, fbs
    finally:
        c.stop()


def test_hover_keyword_agent_msgref():
    c = LspClient()
    try:
        c.notify("textDocument/didOpen", {"textDocument": {"uri": URI,
                "languageId": "parley", "version": 1, "text": EXAMPLE}})
        c.wait_event("textDocument/publishDiagnostics")
        lines = EXAMPLE.splitlines()

        def hover(line, col):
            r = c.request("textDocument/hover", {
                "textDocument": {"uri": URI}, "position": {"line": line, "character": col}})
            res = r["result"]
            return res["contents"]["value"] if res else None

        # keyword: 'request' na linha do ato
        req_line = next(i for i, l in enumerate(lines)
                        if l.strip() == "request route.plan")
        v = hover(req_line, lines[req_line].index("request") + 1)
        assert v and "request" in v and "Abre uma negociação" in v, v

        # agente: 'DISPATCHER' no header
        hdr = next(i for i, l in enumerate(lines) if l.strip().startswith("PLANNER ->"))
        col = lines[hdr].index("DISPATCHER")
        v = hover(hdr, col + 1)
        assert v and "dispatcher-7" in v, v

        # referência: '@msg1' em 'parent "@msg1"'
        par = next(i for i, l in enumerate(lines) if 'parent "@msg1"' in l)
        col = lines[par].index("@msg1")
        v = hover(par, col + 1)
        assert v and "mensagem nº 1" in v, v
    finally:
        c.stop()


def test_document_symbols():
    c = LspClient()
    try:
        c.notify("textDocument/didOpen", {"textDocument": {"uri": URI,
                "languageId": "parley", "version": 1, "text": EXAMPLE}})
        c.wait_event("textDocument/publishDiagnostics")
        r = c.request("textDocument/documentSymbol", {"textDocument": {"uri": URI}})
        syms = r["result"]
        names = {s["name"] for s in syms}
        assert "PLANNER" in names and "BACKUP" in names, names
        assert "route.plan" in names, names
        assert any(n.startswith("msg1:") for n in names), names
        planner = next(s for s in syms if s["name"] == "PLANNER")
        assert planner["detail"] == "planner-42"
        dispatcher = next(s for s in syms if s["name"] == "DISPATCHER")
        assert [ch["name"] for ch in dispatcher["children"]] == ["route.plan"]
    finally:
        c.stop()


def test_server_survives_garbage_and_unknown_method():
    c = LspClient()
    try:
        r = c.request("foo/bar", {})
        assert r["error"]["code"] == -32601
        # server continua vivo: initialize responde
        r = c.request("initialize", {"processId": None, "rootUri": None,
                                     "capabilities": {}})
        assert r["result"]["serverInfo"]["name"] == "parley-lsp"
    finally:
        c.stop()


if __name__ == "__main__":
    import traceback
    fails = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except Exception:
                fails += 1
                print(f"FAIL {name}")
                traceback.print_exc()
    print(f"\n{fails} falhas" if fails else "\nTodos os testes passaram")
    sys.exit(1 if fails else 0)
