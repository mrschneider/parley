#!/usr/bin/env python3
"""
PARLEY v0 — agent-to-agent communication language/protocol.
Domain-specific language, platform-agnostic (HTTP, WebSocket, queues).

Trust model (multi-tenant, untrusted agents):
  - content-addressed message id (sha256) -> idempotency and replay detection
  - signature over the canonical serialization (every field except 'signature')
  - 'tenant' in the envelope, enforceable by the validator
  - provenance: chain of ids backing the message
Hybrid contracts:
  - 'check' clause: machine-validated (path in body, operator, value)
  - 'nl' clause:   written in natural language; only becomes BINDING when the
                    counterparty lists it explicitly in ACCEPT (CLAUSES ...)
"""
from __future__ import annotations
import base64
import hashlib
import hmac
import json
import os
import re
import sys
import tempfile
from datetime import datetime, timezone
from typing import Any

# ---------------------------------------------------------------- ed25519 / hmac
try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    _HAVE_ED25519 = True
except Exception:
    _HAVE_ED25519 = False

def _derive_key(agent_id: str, tenant: str = "default"):
    """Deterministic test key derived from the identity.
    Production: use a KMS / real key wallet — NEVER derive from an id."""
    seed = hashlib.sha256(f"{tenant}:{agent_id}".encode()).digest()
    if _HAVE_ED25519:
        return ("ed25519", Ed25519PrivateKey.from_private_bytes(seed))
    return ("hmac-sha256", seed)

class KeyStore:
    """Wallet of real ed25519 keys (production path).

    The file stores the private seed and must be mode 0600: whoever has the
    file HAS the agent's identity. In production, the ideal path lives on
    encrypted disk / a secret manager — the format is the same."""
    ALG = "ed25519"

    def __init__(self):
        self._keys: dict[tuple[str, str], Ed25519PrivateKey] = {}

    @staticmethod
    def _key(tenant: str, agent_id: str) -> tuple[str, str]:
        return (tenant or "default", agent_id)

    def generate(self, agent_id: str, tenant: str = "default") -> Ed25519PrivateKey:
        if not _HAVE_ED25519:
            raise RuntimeError("cryptography not available for ed25519")
        key = Ed25519PrivateKey.generate()  # type: ignore[misc]
        self._keys[self._key(tenant, agent_id)] = key
        return key

    def put(self, agent_id: str, tenant: str, key: Ed25519PrivateKey) -> None:
        self._keys[self._key(tenant, agent_id)] = key

    def get(self, agent_id: str, tenant: str = "default"):
        return self._keys.get(self._key(tenant, agent_id))

    def public_bytes(self, agent_id: str, tenant: str = "default") -> str:
        key = self.get(agent_id, tenant)
        if key is None:
            raise KeyError(f"missing key: {tenant}/{agent_id}")
        if not _HAVE_ED25519:
            raise RuntimeError("cryptography not available for ed25519")
        pub = key.public_key().public_bytes(
            serialization.Encoding.Raw,  # type: ignore[misc]
            serialization.PublicFormat.Raw)  # type: ignore[misc]
        return base64.b64encode(pub).decode()

    def sign_message(self, msg: dict, agent_id: str, tenant: str = "default") -> dict:
        key = self.get(agent_id, tenant)
        if key is None:
            raise KeyError(f"missing key: {tenant}/{agent_id}")
        out = dict(msg)
        out["signature"] = {
            "alg": self.ALG,
            "key": self.public_bytes(agent_id, tenant),
            "sig": base64.b64encode(key.sign(canonical_bytes(msg))).decode(),
        }
        return out

    def save(self, path: str) -> None:
        if not _HAVE_ED25519:
            raise RuntimeError("cryptography not available for ed25519")
        entries = []
        for (tenant, agent), key in sorted(self._keys.items()):
            seed = key.private_bytes(
                serialization.Encoding.Raw,  # type: ignore[misc]
                serialization.PrivateFormat.Raw,  # type: ignore[misc]
                serialization.NoEncryption())  # type: ignore[misc]
            pub = key.public_key().public_bytes(
                serialization.Encoding.Raw,  # type: ignore[misc]
                serialization.PublicFormat.Raw)  # type: ignore[misc]
            entries.append({"agent": agent, "tenant": tenant, "alg": self.ALG,
                            "seed_b64": base64.b64encode(seed).decode(),
                            "pub_key_b64": base64.b64encode(pub).decode()})
        atomic_write_json(path, entries, mode=0o600)

    def load(self, path: str) -> None:
        try:
            with open(path, encoding="utf-8") as f:
                entries = json.load(f)
        except FileNotFoundError:
            return
        if not _HAVE_ED25519:
            raise RuntimeError("cryptography not available for ed25519")
        for e in entries:
            seed = base64.b64decode(e["seed_b64"])
            self._keys[self._key(e.get("tenant", "default"), e["agent"])] = \
                Ed25519PrivateKey.from_private_bytes(seed)  # type: ignore[misc]

# ---------------------------------------------------------------- serialization
#
# CANONICAL NUMBER RULE (protocol): values are serialized by VALUE, not
# by token — integers as pure integers ("150", never "150.0" nor "1.0"),
# non-integers with the smallest round-trip representation ("0.85"). Python and Go
# MUST follow this rule; it is what makes the canonical bytes (and therefore
# ids and signatures) platform-independent.

def canonical_json(v) -> str:
    """Canonical serialization of any JSON value (canonical number rule)."""
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
    raise TypeError(f"value not canonically serializable: {type(v)!r}")

def canonical_bytes(msg: dict) -> bytes:
    """Canonical serialization: everything except 'signature', sorted keys, compact."""
    body = {k: v for k, v in msg.items() if k != "signature"}
    return canonical_json(body).encode("utf-8")

def atomic_write_json(path: str, value, mode: int = 0o644) -> None:
    """Atomic write with a cross-process lock (sibling .lock file)."""
    lock_path = path + ".lock"
    deadline = datetime.now().timestamp() + 2.0
    while True:
        try:
            fd = os.open(lock_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.close(fd)
            break
        except FileExistsError:
            try:
                age = datetime.now().timestamp() - os.stat(lock_path).st_mtime
                if age > 5:
                    os.unlink(lock_path)
                    continue
            except FileNotFoundError:
                continue
            if datetime.now().timestamp() > deadline:
                raise TimeoutError(f"lock timeout: {lock_path}")
    try:
        directory = os.path.dirname(path) or "."
        fd, tmp = tempfile.mkstemp(prefix=".parley-tmp-", dir=directory)
        try:
            try:
                os.fchmod(fd, mode)
            except (AttributeError, OSError):
                if os.name != "nt":
                    raise
                # Windows does not support fchmod; mkstemp already creates the file user-restricted
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(value, f, ensure_ascii=False, separators=(",", ":"))
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp, path)
        finally:
            if os.path.exists(tmp):
                try:
                    os.unlink(tmp)
                except OSError:
                    if os.name != "nt":
                        raise
    finally:
        try:
            os.unlink(lock_path)
        except FileNotFoundError:
            pass

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
        return False, "missing signature"
    alg, key_id = sig.get("alg"), sig.get("key", "")
    tenant = msg.get("tenant", "default")
    data = canonical_bytes(msg)
    if alg == "ed25519" and _HAVE_ED25519:
        # Production key mode: signature.key is the raw base64 public key.
        try:
            raw_pub = base64.b64decode(key_id, validate=True)
        except Exception:
            raw_pub = b""
        if len(raw_pub) == 32:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
            try:
                Ed25519PublicKey.from_public_bytes(raw_pub).verify(
                    base64.b64decode(sig["sig"]), data)
                return True, "valid signature (ed25519, card key)"
            except Exception as e:
                return False, f"invalid signature: {e}"
    agent_id = key_id.split(":", 1)[1] if ":" in key_id else key_id
    if alg == "ed25519" and _HAVE_ED25519:
        alg2, key = _derive_key(agent_id, tenant)
        try:
            key.public_key().verify(base64.b64decode(sig["sig"]), data)
            return True, "valid signature (ed25519)"
        except Exception as e:
            return False, f"invalid signature: {e}"
    if alg == "hmac-sha256":
        alg2, key = _derive_key(agent_id, tenant)
        expect = hmac.new(key, data, hashlib.sha256).digest()
        if hmac.compare_digest(expect, base64.b64decode(sig["sig"])):
            return True, "valid signature (hmac-sha256)"
        return False, "invalid signature (hmac)"
    return False, f"unsupported algorithm: {alg}"

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
            if s.startswith("["):  # loose list: ["a","b"] or [a, b, c]
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
    cur_prov = None       # capability name being declared
    cur_msg = None        # message under construction
    pending_act = None    # act + target of the act line

    for ln, raw in enumerate(src.splitlines(), 1):
        line = raw.split("#", 1)[0].rstrip() if not raw.lstrip().startswith("#") else ""
        if not line.strip():
            continue
        s = line.strip()
        first = s.split(None, 1)[0].upper()

        # ---- capability declarations -------------------------------------
        if first == "AGENT":
            m = re.match(r'^AGENT\s+"([^"]+)"\s+AS\s+([A-Za-z_][\w-]*)$', s, re.I)
            if not m:
                raise SyntaxError(f"ln {ln}: malformed AGENT: {s}")
            cur_agent = m.group(2)
            doc.agents[cur_agent] = {"id": m.group(1), "provides": []}
            continue
        if first == "PROVIDES":
            m = re.match(r'^PROVIDES\s+"?([a-z0-9_.]+)"?\s+V(\d+)$', s, re.I)
            if not m or not cur_agent:
                raise SyntaxError(f"ln {ln}: malformed PROVIDES: {s}")
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
                raise SyntaxError(f"ln {ln}: malformed INPUT/OUTPUT: {s}")
            kind = "inputs" if first == "INPUT" else "outputs"
            g = m.groups()
            name, typ = (g[0], g[1]) if kind == "inputs" else (g[2], g[3])
            doc.capabilities[cur_prov][kind].append({"name": name, "type": typ.upper()})
            continue
        if first == "COST" and cur_prov:
            m = re.match(r'^COST\s+(\w+)\s*(<=|>=|<|>|==)\s*(.+)$', s, re.I)
            if not m:
                raise SyntaxError(f"ln {ln}: malformed COST: {s}")
            doc.capabilities[cur_prov]["cost"].append(
                {"field": m.group(1), "op": OPS[m.group(2)], "value": parse_value(m.group(3))})
            continue
        if first == "DEFAULT_CONFIDENCE" and cur_prov:
            doc.capabilities[cur_prov]["default_confidence"] = parse_value(s.split(None, 1)[1])
            continue

        # ---- messages -------------------------------------------------------
        hm = HEADER_RE.match(s)
        if hm and hm.group(1) in doc.agents and hm.group(2) in doc.agents:
            cur_msg = _new_msg(doc, hm.group(1), hm.group(2))
            continue
        if cur_msg is None:
            raise SyntaxError(f"ln {ln}: line out of context (agent/message): {s}")
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
                    raise SyntaxError(f"ln {ln}: malformed EXPECT: {s}")
                clause["check"] = {"field": cm.group(1), "op": OPS[cm.group(2)],
                                   "value": parse_value(cm.group(3))}
            clause["weight"] = weight
            m.setdefault("expect", []).append(clause)
            continue
        if first == "CLAUSES":
            m["clauses"] = [x.strip().strip('"') for x in split_top(s.split(None, 1)[1])]
            continue
        raise SyntaxError(f"ln {ln}: unknown token: {first!r} in {s}")

    # stable clause ids + content-addressed message ids
    for m in doc.messages:
        for i, c in enumerate(m.get("expect", []), 1):
            c.setdefault("id", f"c{i}")
        m["_signer"] = m["from"]["id"]
    return doc

def finalize(doc: ParleyDoc) -> list[dict]:
    """Generates content-addressed message ids (idempotency/replay)."""
    for m in doc.messages:
        tmp = {k: v for k, v in m.items() if k not in ("id", "signature")}
        h = hashlib.sha256(canonical_bytes(tmp)).hexdigest()[:16]
        m["id"] = f"msg:{h}"
        m.pop("_signer", None)
    # resolve @msgN references -> id of the Nth message (readable correlation)
    for m in doc.messages:
        if m.get("parent", "").startswith("@msg"):
            n = int(m["parent"][4:])
            m["parent"] = doc.messages[n - 1]["id"]
        if m.get("provenance"):
            m["provenance"] = [doc.messages[int(p[4:]) - 1]["id"] if p.startswith("@msg") else p
                               for p in m["provenance"]]
    return doc.messages

# ---------------------------------------------------------------- validation
def get_path(obj: dict, path: str):
    cur = obj
    for part in path.split("."):
        if isinstance(cur, dict) and part in cur:
            cur = cur[part]
        else:
            return None
    return cur

def check_leaf(chk: dict, msg: dict) -> tuple[bool, str]:
    field: str = chk["field"]
    op: str = chk["op"]
    val: Any = chk.get("value")
    actual: Any = (msg.get("confidence") if field == "confidence"
                   else get_path(msg.get("body", {}), field))
    if op == "not_null":
        ok = actual is not None
    elif op == "in":
        ok = actual in val
    elif op == "regex":
        ok = actual is not None and re.search(val, str(actual)) is not None
    else:
        if actual is None:
            return False, f"missing field: {field}"
        ok = {"lt": actual < val, "lte": actual <= val, "gt": actual > val,
              "gte": actual >= val, "eq": actual == val}[op]
    return ok, f"{field}={actual!r} ({op} {val!r})"

def check_clause(clause: dict, msg: dict) -> tuple[bool, str]:
    chk = clause.get("check")
    if not chk:
        return True, "nl (no machine check)"
    if "all_of" in chk or "any_of" in chk:
        details = {"all_of": [], "any_of": []}
        all_ok, any_ok, present = True, False, False
        for child in chk.get("all_of", []):
            ok, detail = check_clause({"check": child}, msg)
            details["all_of"].append(detail)
            all_ok = all_ok and ok
        if "all_of" in chk:
            present = True
        for child in chk.get("any_of", []):
            ok, detail = check_clause({"check": child}, msg)
            details["any_of"].append(detail)
            any_ok = any_ok or ok
        if "any_of" in chk:
            present = True
        ok = all_ok and (not ("any_of" in chk) or any_ok) if present else False
        return ok, f"all_of={details['all_of']} any_of={details['any_of']}"
    return check_leaf(chk, msg)

# ---------------------------------------------------------------- ranking
#
# clause.weight: "How much this clause matters to the sender (drives
# negotiation ranking)". rank_proposals delivers the promise: given a
# request with weighted 'expect' clauses and the received proposals
# (propose/counter), it returns the ordering by the fraction of contractual
# weight satisfied. Same semantics as the hybrid contracts: reported, never a
# hard rejection — the ranking GUIDES, the requestor decides.
#
#   check ok: +weight | check violated: -weight (violation)
#   nl not accepted: -weight (non-binding) | nl accepted: +weight (binding)
#   weight <= 0: inactive (does not score)
#   score = satisfied/total (0..1); 0 when there is a violation with weight > 0
#
# Deterministic order: score desc, confidence desc, id asc.

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
                continue  # inactive clause
            if "nl" in c:
                cid = c.get("id", "?")
                ok = cid in set(p.get("clauses", []))
                detail = "nl explicitly accepted (binding)" if ok \
                    else "nl not explicitly accepted -> non-binding (penalty)"
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
# Act state machine: a negotiation thread is opened by a
# request (with target) and walks the acts through a minimal grammar,
# with an explicit terminal (accept/commit/reject/cancel) and negotiation
# loops (propose/counter) with a configurable cap. Mirrors the Go.

THREAD_STATES = ("new", "negotiating", "settled", "rejected")
TERMINAL_STATES = ("settled", "rejected")

class Thread:
    """A negotiation: request + the acts that follow it (minimal grammar)."""
    def __init__(self, cap: int = 8):
        self.id = None            # id of the root request
        self.target = None
        self.state = "new"
        self.last_offer = None    # id of the last propose/counter
        self.offer_count = 0
        self.cap = cap if cap > 0 else 8
        self.trace: list[dict] = []

    def _trace(self, m: dict):
        self.trace.append({"act": m.get("act"), "id": m.get("id"),
                           "from": m.get("from", {}).get("id"),
                           "to": m.get("to", {}).get("id"),
                           "state": self.state})

    def apply(self, m: dict) -> None:
        """Transitions the thread with a message. Raises ValueError on the
        first grammar violation (the message is NOT applied)."""
        if m is None:
            raise ValueError("thread: nil message")
        if self.state == "new":
            if m.get("act") != "request" or not m.get("target"):
                raise ValueError(f"thread: open requires 'request' with target "
                                 f"(got '{m.get('act')}' {m.get('id')})")
            self.id, self.target = m["id"], m["target"]
            self.state = "negotiating"
            self._trace(m)
            return
        if self.state in TERMINAL_STATES:
            raise ValueError(f"thread {self.id}: closed in {self.state} — "
                             f"act '{m.get('act')}' ({m.get('id')}) invalid")
        # negotiating
        act = m.get("act")
        if act == "propose":
            self._offer(m)
        elif act == "counter":
            if self.last_offer is None:
                raise ValueError(f"thread: 'counter' requires a previous offer "
                                 f"(thread {self.id}, msg {m.get('id')})")
            self._offer(m)
        elif act in ("accept", "commit"):
            self.state = "settled"
            self._trace(m)
        elif act in ("reject", "cancel"):
            self.state = "rejected"
            self._trace(m)
        elif act in ("delegate", "inform", "ask", "verify"):
            self._trace(m)  # lateral/delegation: no state change
        else:
            raise ValueError(f"thread: unknown act '{act}' (msg {m.get('id')})")

    def _offer(self, m: dict):
        if self.offer_count + 1 > self.cap:
            raise ValueError(f"thread: offer cap exceeded ({self.cap}) — "
                             f"negotiation without end (thread {self.id}, msg {m.get('id')})")
        self.offer_count += 1
        self.last_offer = m["id"]
        self._trace(m)

def thread_from(cap: int, msgs: list[dict]) -> Thread:
    """Builds the thread from the ordered messages (root first).
    Raises ValueError on the first violation."""
    th = Thread(cap)
    for m in msgs:
        th.apply(m)
    return th

# ---------------------------------------------------------------- agreement

def agreement_canonical_bytes(agr: dict) -> bytes:
    tree = {k: v for k, v in agr.items() if k not in ("id", "signatures")}
    return canonical_json(tree).encode("utf-8")

def assign_agreement_id(agr: dict) -> str:
    canon = agreement_canonical_bytes(agr)
    aid = "agreement:" + hashlib.sha256(canon).hexdigest()[:16]
    agr["id"] = aid
    return aid

def build_agreement(request: dict, offer: dict, acceptance: dict) -> dict:
    if not request or not offer or not acceptance:
        raise ValueError("agreement: request/offer/acceptance required")
    if not request.get("id") or not offer.get("id") or not acceptance.get("id"):
        raise ValueError("agreement: messages without content id")
    if request.get("act") != "request":
        raise ValueError(f"agreement: root must be request, got {request.get('act')}")
    if offer.get("act") not in ("propose", "counter"):
        raise ValueError(f"agreement: offer must be propose/counter, got {offer.get('act')}")
    if acceptance.get("act") not in ("accept", "commit"):
        raise ValueError(f"agreement: acceptance must be accept/commit, got {acceptance.get('act')}")
    belongs = acceptance.get("parent") in (request["id"], offer["id"])
    if not belongs:
        # provenance also expresses membership (accept without an explicit parent)
        prov = set(acceptance.get("provenance", []))
        belongs = request["id"] in prov and offer["id"] in prov
    if not belongs:
        raise ValueError(f"agreement: acceptance {acceptance['id']} does not belong to the request/offer thread")
    binding = sorted(acceptance.get("clauses", []))
    tenant = acceptance.get("tenant") or request.get("tenant")
    out = {
        "v": 0,
        "request_id": request["id"],
        "offer_id": offer["id"],
        "acceptance_id": acceptance["id"],
        "act": acceptance["act"],
        "requestor": dict(request["from"]),
        "provider": dict(offer["from"]),
        "binding_clauses": binding,
    }
    if tenant:
        out["tenant"] = tenant
    if request.get("target"):
        out["target"] = request["target"]
    if offer.get("body") is not None:
        out["body"] = dict(offer["body"])
    return out

def sign_agreement(agr: dict, agent: str, keystore: KeyStore, tenant: str = "default") -> dict:
    key = keystore.get(agent, tenant)
    if key is None:
        raise KeyError(f"missing key: {tenant}/{agent}")
    out = dict(agr)
    canon = agreement_canonical_bytes(out)
    sig = {
        "alg": "ed25519",
        "key": keystore.public_bytes(agent, tenant),
        "sig": base64.b64encode(key.sign(canon)).decode(),
    }
    sigs = [s for s in out.get("signatures", []) if s.get("agent") != agent]
    sigs.append({"agent": agent, "signature": sig})
    sigs.sort(key=lambda x: x["agent"])
    out["signatures"] = sigs
    assign_agreement_id(out)
    return out

def verify_agreement(agr: dict) -> tuple[bool, str]:
    aid = agr.get("id", "")
    if not aid:
        return False, "agreement without id"
    canon = agreement_canonical_bytes(agr)
    expected = "agreement:" + hashlib.sha256(canon).hexdigest()[:16]
    if expected != aid:
        return False, "content id mismatch"
    sigs = agr.get("signatures", [])
    if not sigs:
        return False, "agreement without signatures"
    seen = set()
    for s in sigs:
        agent = s.get("agent", "")
        if agent in seen:
            return False, f"duplicate signature: {agent}"
        seen.add(agent)
        sig = s.get("signature")
        if not sig:
            return False, f"missing signature: {agent}"
        if not _HAVE_ED25519:
            return False, "cryptography not available for ed25519"
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
        raw_pub = base64.b64decode(sig["key"])
        try:
            Ed25519PublicKey.from_public_bytes(raw_pub).verify(
                base64.b64decode(sig["sig"]), canon)
        except Exception as e:
            return False, f"invalid signature ({agent}): {e}"
    return True, "agreement valid"

def validate(msg: dict, parent: dict | None = None, now: datetime | None = None) -> dict:
    """Returns {schema_ok, signature_ok, sig_msg, tenant_ok, clauses: [...], warnings: [...]}. """
    from jsonschema import validate as jsval, ValidationError
    out = {"id": msg.get("id"), "act": msg.get("act"), "warnings": []}

    schema_path = __file__.replace(
        "parley.py", f"schema/parley-v{'1' if int(msg.get('v', 0) or 0) >= 1 else '0'}.schema.json")
    schema = json.load(open(schema_path))
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
        out["warnings"].append("missing tenant (required in multi-tenant deployments)")

    now = now or datetime.now(timezone.utc)
    dl = msg.get("deadline")
    if dl and datetime.strptime(dl, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc) < now:
        out["warnings"].append(f"expired deadline: {dl}")

    # contract: 'expect' clauses of the PARENT message evaluated against THIS message
    if parent and "expect" in parent and msg["act"] in ("propose", "counter", "accept", "commit"):
        accepted = set(msg.get("clauses", []))
        out["clauses"] = []
        for c in parent["expect"]:
            cid = c.get("id", "?")
            if "nl" in c:
                binding = cid in accepted
                out["clauses"].append({"id": cid, "nl": c["nl"], "binding": binding,
                                       "detail": "explicitly accepted" if binding
                                       else "not explicitly accepted -> non-binding"})
            else:
                okc, detail = check_clause(c, msg)
                out["clauses"].append({"id": cid, "check": c["check"], "ok": okc, "detail": detail})
    return out

# ---------------------------------------------------------------- CLI
def main():
    if len(sys.argv) < 2:
        print("usage: parley.py <file.parley> [--sign] [--thread]")
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