#!/usr/bin/env python3
"""PARLEY performance benchmark — hot paths of the reference implementation.

Usage: python3 bench_perf.py
Prints one line per scenario: name, iterations, total ms, avg us/op.
Designed to be run before/after optimizations; keep it in the repo.
"""
from __future__ import annotations
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import parley  # noqa: E402
import parley_lsp  # noqa: E402


def make_msg(i: int) -> dict:
    return {
        "v": 0,
        "from": {"id": f"agent-{i % 7}"},
        "to": {"id": f"agent-{(i + 3) % 7}"},
        "ts": "2026-11-16T12:00:00Z",
        "act": "propose",
        "target": "route.plan",
        "tenant": "acme",
        "confidence": 0.9,
        "body": {
            "route": {"eta_min": 140 + i % 20, "distance_km": 310.5,
                      "legs": [{"km": 12.5, "mode": "road"} for _ in range(4)]},
            "origin": "Lisbon", "dest": "Porto",
        },
        "expect": [
            {"id": "c1", "check": {"field": "confidence", "op": "gt", "value": 0.85}, "weight": 1.0},
            {"id": "c2", "check": {"field": "route.eta_min", "op": "lte", "value": 150}, "weight": 0.9},
            {"id": "c3", "nl": "Avoid roadworks in the next 24h", "weight": 0.9},
        ],
    }


def bench(name, fn, iters):
    t0 = time.perf_counter()
    for _ in range(iters):
        fn()
    dt = (time.perf_counter() - t0) * 1000.0
    print(f"{name:34s} iters={iters:<6d} total={dt:10.1f} ms  avg={dt * 1000 / iters:9.2f} us/op")


def main():
    msg = make_msg(1)
    big = make_msg(2)
    big["body"]["route"]["legs"] = [{"km": 1.5, "mode": "road", "tags": ["a", "b"]} for _ in range(200)]

    # canonical serialization (core of ids/signatures)
    bench("canonical_json(msg)", lambda: parley.canonical_json(msg), 20000)
    bench("canonical_json(big)", lambda: parley.canonical_json(big), 5000)
    bench("canonical_bytes(msg)", lambda: parley.canonical_bytes(msg), 20000)

    # signing / verification (test-mode derived keys)
    bench("sign(msg)", lambda: parley.sign(msg, "agent-1", "acme"), 3000)
    signed = parley.sign(msg, "agent-1", "acme")
    bench("verify(signed)", lambda: parley.verify(signed), 3000)

    # schema validation (validate() loads+compiles the schema every call)
    bench("validate(msg)", lambda: parley.validate(signed, now=None), 500)

    # parser
    src_lines = []
    for i in range(20):
        src_lines += [
            f'agent "agent-{i}" as A{i}',
            f'agent "agent-{(i + 1) % 20}" as B{i}',
            f'A{i} -> B{i}',
            '  request route.plan',
            '  intent "Fastest route"',
            '  with origin = "Lisbon", dest = "Porto"',
            '  by 2026-11-16T15:00:00Z',
            '  expect confidence > 0.85',
            '  expect route.eta_min <= 150 w 0.9',
            '  expect nl "Avoid roadworks" w 0.9',
        ]
    src = "\n".join(src_lines)
    bench("parse_text(20 msgs)", lambda: parley.parse_text(src), 300)

    # ranking
    parent = make_msg(0)
    parent["act"] = "request"
    parent["id"] = "msg:0000000000000001"
    proposals = [parley.sign(make_msg(i), "agent-1", "acme") for i in range(50)]
    for p, i in zip(proposals, range(50)):
        p["id"] = f"msg:{i:016x}"
        p["clauses"] = ["c3"] if i % 2 else []
    bench("rank_proposals(50)", lambda: parley.rank_proposals(parent, proposals), 500)

    # LSP analysis (tolerant scan + parser + semantics)
    bench("lsp.analyze(20 msgs)", lambda: parley_lsp.analyze(src), 200)
    model = parley_lsp.analyze(src)
    bench("lsp.context_at", lambda: parley_lsp.context_at(model, 5), 20000)

    # split_top (used by WITH/PROVENANCE/CLAUSES)
    s = ", ".join(f'k{i} = "v{i}"' for i in range(30))
    bench("split_top(30 parts)", lambda: parley.split_top(s), 20000)


if __name__ == "__main__":
    main()
