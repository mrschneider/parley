#!/usr/bin/env python3
"""PARLEY v0 — Python reference tests (weight ranking + thread).

Run: python3 -m pytest test_parley.py -q   (or: python3 test_parley.py)
"""
from __future__ import annotations
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from parley import (parse_text, finalize, sign, rank_proposals, Thread,
                    thread_from, Thread as _T,  # noqa: F401
                    KeyStore, verify, check_clause, validate,
                    build_agreement, sign_agreement, verify_agreement,
                    _HAVE_ED25519)  # noqa: E402

T0 = "2026-10-23T09:00:00Z"


def _msg(frm, to, act, target=None, parent=None, body=None, conf=None,
         clauses=None, ts="2026-10-23T09:01:00Z"):
    m = {"v": 0, "from": {"id": frm}, "to": {"id": to}, "ts": ts,
         "act": act, "tenant": "acme"}
    if target:
        m["target"] = target
    if parent:
        m["parent"] = parent
    if body is not None:
        m["body"] = body
    if conf is not None:
        m["confidence"] = conf
    if clauses is not None:
        m["clauses"] = clauses
    return m


def _with_id(m):
    """Assigns a content id (mirrors the Go AssignID)."""
    import hashlib
    from parley import canonical_bytes
    tmp = {k: v for k, v in m.items() if k not in ("id", "signature")}
    m["id"] = "msg:" + hashlib.sha256(canonical_bytes(tmp)).hexdigest()[:16]
    return m


def parent_with_weights():
    """request with a weighted contract: c1 conf>0.85 (1.0),
    c2 route.eta_min<=150 (0.9), c3 nl (0.9). Total 2.8."""
    p = _msg("planner-42", "dispatcher-7", "request", target="route.plan",
             body={"origin": "Lisboa", "dest": "Porto"})
    p["expect"] = [
        {"id": "c1", "check": {"field": "confidence", "op": "gt", "value": 0.85}, "weight": 1.0},
        {"id": "c2", "check": {"field": "route.eta_min", "op": "lte", "value": 150}, "weight": 0.9},
        {"id": "c3", "nl": "avoid roadworks in the next 24h", "weight": 0.9},
    ]
    return _with_id(p)


# ---------------------------------------------------------------- ranking

def test_ranking_order():
    p = parent_with_weights()
    a = _with_id(_msg("dispatcher-7", "planner-42", "propose", target="route.plan",
                      parent=p["id"],
                      body={"route": {"eta_min": 142}}, conf=0.93))
    b = _with_id(_msg("dispatcher-7", "planner-42", "propose", target="route.plan",
                      parent=p["id"],
                      body={"route": {"eta_min": 100}}, conf=0.95,
                      clauses=["c3"]))
    c = _with_id(_msg("dispatcher-7", "planner-42", "propose", target="route.plan",
                      parent=p["id"],
                      body={"route": {"eta_min": 200}}, conf=0.99,
                      clauses=["c3"]))
    r = rank_proposals(p, [a, b, c])
    assert len(r["entries"]) == 3
    assert abs(r["total"] - 2.8) < 1e-9
    assert r["top"]["proposal"]["id"] == b["id"]
    assert abs(r["top"]["score"] - 1.0) < 1e-9
    by = {e["proposal"]["id"]: e["score"] for e in r["entries"]}
    assert by[a["id"]] == 0.0  # nl not accepted -> violation -> 0
    assert by[c["id"]] == 0.0  # c2 violated -> 0


def test_ranking_tiebreak_confidence():
    p = parent_with_weights()
    p1 = _with_id(_msg("d", "planner-42", "propose", parent=p["id"],
                       body={"route": {"eta_min": 200}}, conf=0.99, clauses=["c3"]))
    q = _with_id(_msg("d", "planner-42", "propose", parent=p["id"],
                      body={"route": {"eta_min": 100}}, conf=0.90))
    r = rank_proposals(p, [q, p1])
    assert all(e["score"] == 0.0 for e in r["entries"])
    assert r["entries"][0]["proposal"]["id"] == p1["id"]  # conf 0.99 > 0.90


def test_ranking_inert_clause():
    p = _msg("a", "b", "request")
    p["expect"] = [
        {"id": "x", "check": {"field": "confidence", "op": "gt", "value": 0.85}, "weight": 1.0},
        {"id": "y", "nl": "inactive clause", "weight": 0.0},
    ]
    _with_id(p)
    off = _with_id(_msg("b", "a", "propose", parent=p["id"], conf=0.9))
    r = rank_proposals(p, [off])
    assert abs(r["total"] - 1.0) < 1e-9
    assert r["top"] is not None
    assert abs(r["top"]["score"] - 1.0) < 1e-9


def test_ranking_no_offers():
    p = parent_with_weights()
    r = rank_proposals(p, [])
    assert r["entries"] == [] and r["top"] is None
    acc = _with_id(_msg("b", "a", "accept", parent=p["id"]))
    r2 = rank_proposals(p, [acc])
    assert r2["entries"] == []  # accept is not an offer


# ---------------------------------------------------------------- thread

def test_thread_happy_path():
    th = Thread()
    req = _with_id(_msg("planner-42", "dispatcher-7", "request", target="route.plan"))
    th.apply(req)
    assert th.state == "negotiating" and th.id == req["id"] and th.target == "route.plan"
    prop = _with_id(_msg("dispatcher-7", "planner-42", "propose",
                         target="route.plan", parent=req["id"]))
    th.apply(prop)
    assert th.last_offer == prop["id"] and th.offer_count == 1
    cn = _with_id(_msg("planner-42", "dispatcher-7", "counter",
                       target="route.plan", parent=prop["id"]))
    th.apply(cn)
    assert th.offer_count == 2
    acc = _with_id(_msg("dispatcher-7", "planner-42", "accept",
                        target="route.plan", parent=cn["id"]))
    th.apply(acc)
    assert th.state == "settled"
    late = _with_id(_msg("planner-42", "dispatcher-7", "propose",
                         target="route.plan", parent=acc["id"]))
    try:
        th.apply(late)
        raise AssertionError("act after settle should fail")
    except ValueError:
        pass
    assert len(th.trace) == 4


def test_thread_open_requires_request():
    th = Thread()
    for act in ("inform", "ask"):
        try:
            th.apply(_with_id(_msg("a", "b", act)))
            raise AssertionError(f"{act} should not open a thread")
        except ValueError:
            pass
    try:
        th.apply(_with_id(_msg("a", "b", "request")))  # without target
        raise AssertionError("request without target should not open")
    except ValueError:
        pass
    assert th.state == "new"


def test_thread_counter_without_offer():
    th = Thread()
    req = _with_id(_msg("a", "b", "request", target="route.plan"))
    th.apply(req)
    cn = _with_id(_msg("b", "a", "counter", target="route.plan", parent=req["id"]))
    try:
        th.apply(cn)
        raise AssertionError("counter without offer should fail")
    except ValueError:
        pass


def test_thread_reject_and_cancel_end():
    for act in ("reject", "cancel"):
        th = Thread()
        req = _with_id(_msg("a", "b", "request", target="route.plan"))
        th.apply(req)
        m = _with_id(_msg("b", "a", act, target="route.plan", parent=req["id"]))
        th.apply(m)
        assert th.state == "rejected"
        prop = _with_id(_msg("b", "a", "propose", target="route.plan", parent=m["id"]))
        try:
            th.apply(prop)
            raise AssertionError(f"propose after {act} should fail")
        except ValueError:
            pass


def test_thread_delegate_keeps_going():
    th = Thread()
    req = _with_id(_msg("a", "b", "request", target="route.plan"))
    th.apply(req)
    dl = _with_id(_msg("b", "c", "delegate", target="route.plan", parent=req["id"]))
    th.apply(dl)
    assert th.state == "negotiating"
    prop = _with_id(_msg("c", "a", "propose", target="route.plan", parent=dl["id"]))
    th.apply(prop)
    assert th.last_offer == prop["id"]


def test_thread_loop_cap():
    th = Thread(cap=2)
    req = _with_id(_msg("a", "b", "request", target="route.plan"))
    th.apply(req)
    last = req["id"]
    for i, act in enumerate(("propose", "counter"), 1):
        om = _with_id(_msg("b", "a", act, target="route.plan", parent=last))
        th.apply(om)
        last = om["id"]
    over = _with_id(_msg("b", "a", "counter", target="route.plan", parent=last))
    try:
        th.apply(over)
        raise AssertionError("offer beyond the cap should fail")
    except ValueError:
        pass
    assert th.offer_count == 2


def test_thread_from_example_file():
    """The negotiation in examples/negotiation.parley is a valid thread:
    request -> propose -> accept (settled)."""
    src = open(os.path.join(os.path.dirname(__file__),
                            "examples", "negotiation.parley")).read()
    doc = parse_text(src)
    msgs = finalize(doc)
    th = thread_from(8, msgs)
    assert th.state == "settled"
    assert [s["act"] for s in th.trace] == ["request", "propose", "accept"]


def test_thread_trace_states():
    th = Thread()
    req = _with_id(_msg("a", "b", "request", target="route.plan"))
    prop = _with_id(_msg("b", "a", "propose", target="route.plan", parent=req["id"]))
    th = thread_from(8, [req, prop])
    assert th.trace[0]["state"] == "negotiating"
    assert th.trace[1]["state"] == "negotiating"


def test_ranking_interop_fixture_matches_go():
    """Interop: the SAME fixture validated by Go (TestInteropRankingGoMatchesPython)
    must produce the SAME ordering and scores in Python. The weight-ranking is
    part of the protocol, not the SDK — both sides converge on the fixture."""
    fx_path = os.path.join(os.path.dirname(__file__),
                           "sdk", "go", "testdata", "interop-ranking.json")
    if not os.path.exists(fx_path):
        return  # fixture missing (same skip policy as the Go side)
    fx = json.load(open(fx_path))
    r = rank_proposals(fx["parent"], fx["proposals"])
    assert len(r["entries"]) == len(fx["expected"]), \
        f"entries = {len(r['entries'])}, expected {len(fx['expected'])}"
    for i, ex in enumerate(fx["expected"]):
        got = r["entries"][i]
        assert got["proposal"]["id"] == ex["id"], \
            f"order[{i}] = {got['proposal']['id']}, expected {ex['id']}"
        assert abs(got["score"] - ex["score"]) < 1e-9, \
            f"score[{i}] = {got['score']}, expected {ex['score']}"


# ---------------------------------------------------------------- keystore / agreements / v1

def test_keystore_real_keys_sign_and_reload(tmp_path=None):
    import tempfile, os, stat
    if not _HAVE_ED25519:
        return  # cryptography missing: production path unavailable
    tmp = tmp_path or tempfile.mkdtemp()
    path = os.path.join(tmp, "keys.json")
    ks = KeyStore()
    ks.generate("worker-1", "acme")
    ks.save(path)
    if os.name != "nt":  # POSIX permissions; Windows does not express them in st_mode
        assert stat.S_IMODE(os.stat(path).st_mode) == 0o600

    m = _with_id(_msg("worker-1", "planner-1", "inform"))
    signed = ks.sign_message(m, "worker-1", "acme")
    ok, why = verify(signed)
    assert ok and "card key" in why, why

    reloaded = KeyStore()
    reloaded.load(path)
    signed2 = reloaded.sign_message(m, "worker-1", "acme")
    assert signed2["signature"]["sig"] == signed["signature"]["sig"]

    # tamper: the body changes -> id changes -> signature fails
    tampered = dict(signed, body={"x": 1})
    ok, _ = verify(tampered)
    assert not ok


def test_composite_checks_v1_schema():
    chk = {"check": {"all_of": [
        {"field": "route.eta_min", "op": "lte", "value": 150},
        {"any_of": [
            {"field": "route.toll_free", "op": "eq", "value": True},
            {"field": "route.eta_min", "op": "lte", "value": 80},
        ]},
    ]}}
    ok, _ = check_clause(chk, {"body": {"route": {"eta_min": 100, "toll_free": True}}})
    assert ok
    ok, _ = check_clause(chk, {"body": {"route": {"eta_min": 151, "toll_free": True}}})
    assert not ok          # all_of failed
    ok, _ = check_clause(chk, {"body": {"route": {"eta_min": 140, "toll_free": False}}})
    assert not ok          # any_of with no active branch

    # v1 accepts composite; v0 rejects
    m = _with_id(_msg("planner-42", "dispatcher-7", "propose", target="route.plan"))
    m["v"] = 1
    m["expect"] = [dict(chk, id="composite", weight=1)]
    if _HAVE_ED25519:
        m = sign(m, "planner-42", "acme")
    r1 = validate(m)
    assert r1["schema_ok"], r1.get("schema_error")
    m0 = dict(m, v=0)
    r0 = validate(m0)
    assert not r0["schema_ok"]


def test_agreement_build_sign_verify_and_tamper():
    if not _HAVE_ED25519:
        return
    req = _with_id(_msg("planner-42", "dispatcher-7", "request",
                        target="route.plan", body={"origin": "Lisboa"}))
    offer = _with_id(_msg("dispatcher-7", "planner-42", "propose",
                          target="route.plan", parent=req["id"],
                          body={"route": {"eta_min": 100}}, clauses=["c3"]))
    acc = _with_id(_msg("planner-42", "dispatcher-7", "commit",
                        target="route.plan", parent=offer["id"], clauses=["c3"]))
    agr = build_agreement(req, offer, acc)
    assert agr["target"] == "route.plan" and agr["act"] == "commit"
    assert agr["binding_clauses"] == ["c3"]
    assert agr["provider"]["id"] == "dispatcher-7"
    assert "id" not in agr and "signatures" not in agr

    # thread mismatch must fail
    bad = dict(acc, parent="msg:0000000000000000")
    try:
        build_agreement(req, offer, bad)
        raise AssertionError("thread mismatch accepted")
    except ValueError:
        pass

    ks = KeyStore()
    ks.generate("planner-42", "acme")
    ks.generate("dispatcher-7", "acme")
    single = sign_agreement(agr, "planner-42", ks, "acme")
    ok, why = verify_agreement(single)
    assert ok, why

    # tampering invalidates the content id
    tampered = dict(single, binding_clauses=["c1", "c3"])
    ok, why = verify_agreement(tampered)
    assert not ok and why == "content id mismatch"

    both = sign_agreement(single, "dispatcher-7", ks, "acme")
    assert len(both["signatures"]) == 2
    ok, why = verify_agreement(both)
    assert ok, why


# ---------------------------------------------------------------- main

def main():
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_")]
    failed = 0
    for fn in fns:
        try:
            fn()
            print(f"PASS {fn.__name__}")
        except Exception as e:  # noqa: BLE001
            failed += 1
            print(f"FAIL {fn.__name__}: {e!r}")
    print(f"\n{len(fns) - failed}/{len(fns)} tests passed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()