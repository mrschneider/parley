// PARLEY v0 (Go) — end-to-end negotiation demo (multi-tenant, untrusted).
// Mirror of demo.py: validates the 3 messages, verifies signatures,
// the hybrid contract (check + nl) and proves tamper-proofing.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"example.org/parley-go"
	"example.org/parley-go/transport"
)

var NOW = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func banner(t string) {
	fmt.Println("\n" + "============================================================================" +
		"\n" + t + "\n" + "==========================================================================")
}

func show(label string, m *parley.Message) {
	slim := map[string]any{}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &slim)
	if sig, ok := slim["signature"].(map[string]any); ok {
		if s, ok := sig["sig"].(string); ok && len(s) > 24 {
			sig["sig"] = s[:24] + "..."
		}
	}
	out, _ := json.MarshalIndent(slim, "", "  ")
	fmt.Printf("\n--- %s ---\n%s\n", label, out)
}

func pretty(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func readExample() string {
	for _, p := range []string{
		"examples/negotiation.parley",             // run from the repository root
		"sdk/go/demo/examples/negotiation.parley", // run from the repository root (fallback)
		"../../examples/negotiation.parley",       // run from sdk/go (go run ./demo)
		"../../../examples/negotiation.parley",    // last resort: from the root
	} {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	fmt.Fprintln(os.Stderr, "error: examples/negotiation.parley not found")
	os.Exit(1)
	return ""
}

func main() {
	src := readExample()
	doc, err := parley.ParseText(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "syntax:", err)
		os.Exit(1)
	}
	msgs := parley.Finalize(doc)
	for i, m := range msgs {
		msgs[i] = parley.Sign(m, m.From.ID, m.Tenant)
	}
	m1, m2, m3 := msgs[0], msgs[1], msgs[2]

	banner("1) VALIDATION (message 1: PLANNER request)")
	r1 := parley.Validate(m1, nil, NOW)
	pretty(r1)

	banner("2) VALIDATION (message 2: DISPATCHER propose against msg 1's contract)")
	r2 := parley.Validate(m2, m1, NOW)
	pretty(r2)

	banner("3) VALIDATION (message 3: PLANNER selective accept)")
	r3 := parley.Validate(m3, m1, NOW)
	pretty(r3)

	banner("4) TAMPER PROOF: message 2 with tampered body")
	bad := *m2
	bad.Body = map[string]any{}
	for k, v := range m2.Body {
		bad.Body[k] = v
	}
	bad.Body["eta_min"] = 240 // does not violate 'route.eta_min <= 150' (route.* path)
	conf := 0.5
	bad.Confidence = &conf // violates 'confidence > 0.85'
	ok, why := parley.Verify(&bad)
	r2b := parley.Validate(&bad, m1, NOW)
	fmt.Printf("signature: %v (%s)\n", ok, why)
	pretty(r2b["clauses"])

	banner("6) BODY CONTRACT: body validated against the capability (route.plan)")
	// The capability declared in the .parley becomes a body contract:
	// request -> INPUTS (origin/dest required), propose -> OUTPUTS (route).
	capRoute := doc.Capabilities["route.plan"]
	contracts := map[string]*parley.Capability{"route.plan": &capRoute}

	// 6a) legitimate request: body has origin+dest -> input contract OK
	r1c := parley.ValidateWith(m1, nil, NOW, contracts)
	fmt.Printf("request  (inputs):  body_contract.ok=%v  (%v)\n",
		r1c["body_contract"].(map[string]any)["ok"],
		r1c["body_contract"].(map[string]any)["capability"])

	// 6b) request with 'dest' missing: violation reported (not rejected)
	badReq := *m1
	badReq.Body = map[string]any{"origin": "Lisboa"} // no dest
	parley.AssignID(&badReq)
	badReq = *parley.Sign(&badReq, "planner-42", "acme")
	r1b := parley.ValidateWith(&badReq, nil, NOW, contracts)
	bcBad := r1b["body_contract"].(map[string]any)
	fmt.Printf("request  (dest missing): ok=%v  violations=%v\n",
		bcBad["ok"], bcBad["violations"])

	// 6c) legitimate propose: body has 'route' -> output contract OK
	r2c := parley.ValidateWith(m2, m1, NOW, contracts)
	fmt.Printf("propose  (outputs):  body_contract.ok=%v\n",
		r2c["body_contract"].(map[string]any)["ok"])

	banner("7) SUPERVISOR: deadline + fallback with runtime enforcement")
	// m1 has a deadline (2026-10-22T15:00:00Z) + fallback (dispatcher-backup).
	// The Supervisor is the watcher that MAKES the fallback happen: without
	// it the deadline would be just a warning.
	sup := transport.NewSupervisor().WithClock(func() time.Time { return NOW })
	fbInbox := transport.NewInbox()
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *transport.Result {
		return fbInbox.Deliver(b)
	})
	m1raw, _ := json.Marshal(m1)
	dl, _ := sup.Track(m1raw)
	fmt.Printf("escrow: %s  deadline=%s  pending=%v\n", m1.ID, dl.Format("15:04:05"), sup.Pending())

	// 7a) primary replies in time -> escalation canceled
	sup.Reply(m1.ID, m2.ID)
	fmt.Printf("primary replied (propose): pending=%v  escalations=%d\n",
		sup.Pending(), len(sup.SettleLog()))

	// 7b) timeout scenario: new request, primary silent, deadline passes
	req2 := parley.NewMessage("planner-42", "dispatcher-7", NOW)
	req2.Tenant = "acme"
	req2.Act = "request"
	req2.Target = "route.plan"
	req2.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	req2.Deadline = NOW.Add(5 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	req2.Fallback = &parley.Fallback{To: parley.AgentRef{ID: "dispatcher-backup"}, Reason: "timeout"}
	parley.AssignID(req2)
	req2 = parley.Sign(req2, "planner-42", "acme")
	req2raw, _ := json.Marshal(req2)
	sup.Track(req2raw)
	fmt.Printf("new request %s (deadline +5min), primary silent...\n", req2.ID)

	// advance the clock past the deadline
	sup.WithClock(func() time.Time { return NOW.Add(6 * time.Minute) })
	del := sup.Tick()
	if len(del) == 1 {
		fmt.Printf("escalated to fallback: to=%s reason=%s  (fallback inbox: %d msg)\n",
			del[0].To, del[0].Reason, fbInbox.Len())
	} else {
		fmt.Printf("WARNING: expected 1 escalation, got %d\n", len(del))
	}

	// 7c) the sender re-targets + re-signs the envelope for the fallback
	esc := transport.DeliverFallback(req2)
	esc.To = parley.AgentRef{ID: "dispatcher-backup"}
	parley.AssignID(esc)
	esc = parley.Sign(esc, "planner-42", "acme")
	escraw, _ := json.Marshal(esc)
	rEsc := fbInbox.Deliver(escraw)
	fmt.Printf("escalation re-target+re-sign accepted in the fallback inbox: %s (%s)\n",
		rEsc.Status, rEsc.Reason)

	banner("8) WEIGHT RANKING: two competing proposals for the SAME request")
	// A: conf 0.93, eta 142, does NOT accept the NL clause -> penalized (score 0)
	// B: conf 0.95, eta 100, ACCEPTS the NL (c3) -> satisfies everything (score 1.0)
	buildOffer := func(agent string, ts time.Time, eta int, conf float64, clauses []string) *parley.Message {
		m := parley.NewMessage(agent, "planner-42", ts)
		m.Tenant = "acme"
		m.Act = "propose"
		m.Target = "route.plan"
		m.Parent = m1.ID
		m.Body = map[string]any{"route": map[string]any{"eta_min": eta}}
		c := conf
		m.Confidence = &c
		m.Clauses = clauses
		parley.AssignID(m)
		return parley.Sign(m, agent, "acme")
	}
	offA := buildOffer("dispatcher-7", NOW.Add(time.Minute), 142, 0.93, nil)
	offB := buildOffer("dispatcher-8", NOW.Add(2*time.Minute), 100, 0.95, []string{"c3"})
	rk := parley.RankProposals(m1, []*parley.Message{offA, offB})
	for _, e := range rk.Entries {
		fmt.Printf("  %s: score=%.2f  ok=%v viol=%v\n",
			e.Proposal.From.ID, e.Score, e.Satisfied, e.Violations)
	}
	fmt.Printf("top: %s (score %.2f) — accepts the NL clause (binding) and satisfies the checks\n",
		rk.Top.Proposal.From.ID, rk.Top.Score)

	banner("9) THREAD: the acts' state machine over the negotiation")
	th, err := parley.NewThreadFrom(8, []*parley.Message{m1, m2, m3})
	if err != nil {
		fmt.Fprintln(os.Stderr, "thread:", err)
		os.Exit(1)
	}
	for _, s := range th.Trace {
		fmt.Printf("  %-8s %s  -> state %s\n", s.Act, s.ID, s.State)
	}
	fmt.Printf("final state: %s (accept ended it; offers=%d cap=%d)\n",
		th.State, th.OfferCount, th.Cap)

	banner("10) PRODUCTION RUNTIME: real keys + registry + complete fallback")
	// Unlike test mode (key derived from the id), here the keys are REAL
	// ed25519 keys in the keystore, the cards publish the public key to the
	// registry, and the fallback inbox trusts the registry
	// (Registry.VerifyMessageWithCard).
	keyDir, _ := os.MkdirTemp("", "parley-demo-")
	defer os.RemoveAll(keyDir)
	agents := []string{"planner-42", "dispatcher-7", "dispatcher-backup"}
	ks, pubs, err := parley.LoadOrGenerate(keyDir+"/keys.json", "acme", agents)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keystore:", err)
		os.Exit(1)
	}
	registry := parley.NewRegistry()
	for _, agent := range agents {
		card := &parley.Card{
			V: parley.Version, Agent: agent, Tenant: "acme",
			Endpoint: "runtime://" + agent, Transport: "queue",
			Pub: parley.CardPubKey{Alg: "ed25519",
				Key: base64.StdEncoding.EncodeToString(pubs[agent])},
			Provides: []parley.CardCapability{{Name: "route.plan", Version: 1}},
		}
		card = parley.SignCardWith(card, ks.MustGet(agent, "acme"))
		if err := registry.Publish(card, agent, NOW); err != nil {
			fmt.Fprintln(os.Stderr, "registry:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("registry: %d cards published (real keys, verified against card.Pub)\n",
		len(registry.List("acme")))

	prodReq := parley.NewMessage("planner-42", "dispatcher-7", NOW)
	prodReq.Tenant = "acme"
	prodReq.Act = "request"
	prodReq.Target = "route.plan"
	prodReq.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	prodReq.Deadline = NOW.Add(5 * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
	prodReq.Fallback = &parley.Fallback{To: parley.AgentRef{ID: "dispatcher-backup"}, Reason: "timeout"}
	parley.AssignID(prodReq)
	prodReq, err = ks.SignWith(prodReq, "acme")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
	prodRaw, _ := json.Marshal(prodReq)

	prodInbox := transport.NewInbox(transport.WithCardRegistry(registry))
	fbProdInbox := transport.NewInbox(transport.WithCardRegistry(registry))
	prodSup := transport.NewSupervisor().WithClock(func() time.Time { return NOW })
	completeSink := transport.CompleteFallbackSink(ks, "acme",
		map[string]*transport.Inbox{"dispatcher-backup": fbProdInbox})
	var fbRes *transport.Result
	prodSup.RegisterSink("dispatcher-backup", func(to string, raw []byte) *transport.Result {
		fbRes = completeSink(to, raw)
		return fbRes
	})

	resProd := prodInbox.Deliver(prodRaw)
	fmt.Printf("inbox with registry trust: %s (%s) | signature via card: ", resProd.Status, resProd.Reason)
	okCard, whyCard := registry.VerifyMessageWithCard(prodReq)
	fmt.Printf("%v (%s)\n", okCard, whyCard)

	prodSup.Track(prodRaw)
	prodSup.WithClock(func() time.Time { return NOW.Add(6 * time.Minute) })
	prodDel := prodSup.Tick()
	prodAccepted := false
	if len(prodDel) == 1 && fbRes != nil && fbRes.Status == transport.StatusAccepted {
		if em, ok := fbProdInbox.Get(fbRes.ID); ok {
			escOK, escWhy := registry.VerifyMessageWithCard(em)
			prodAccepted = escOK
			fmt.Printf("automatic escalation: to=%s provenance=%d signature re-verified against card=%v (%s)\n",
				em.To.ID, len(em.Provenance), escOK, escWhy)
		}
	} else {
		fmt.Println("WARNING: production escalation not confirmed")
	}

	banner("11) AGREEMENT: canonical receipt of the negotiation, signed by both sides")
	// The demo's accept materializes the agreement: who won, NL clauses that
	// became binding and the accepted body — auditable without replaying the thread.
	agr, err := parley.BuildAgreement(m1, m2, m3)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agreement:", err)
		os.Exit(1)
	}
	ksAgr, _, err := parley.LoadOrGenerate(keyDir+"/agr-keys.json", "acme",
		[]string{"planner-42", "dispatcher-7"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "keystore:", err)
		os.Exit(1)
	}
	agr, err = parley.SignAgreement(agr, "planner-42", "acme", ksAgr.MustGet("planner-42", "acme"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign agreement:", err)
		os.Exit(1)
	}
	agrOK1, agrWhy1 := parley.VerifyAgreement(agr)
	agr, err = parley.SignAgreement(agr, "dispatcher-7", "acme", ksAgr.MustGet("dispatcher-7", "acme"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign agreement:", err)
		os.Exit(1)
	}
	agrOK2, agrWhy2 := parley.VerifyAgreement(agr)
	fmt.Printf("agreement id=%s act=%s provider=%s binding=%v signatures=%d single=%v (%s) dual=%v (%s)\n",
		agr.ID, agr.Act, agr.Provider.ID, agr.BindingClauses, len(agr.Signatures),
		agrOK1, agrWhy1, agrOK2, agrWhy2)

	// tampering with the accepted body invalidates the content id
	agrTampered := *agr
	agrTampered.Body = map[string]any{"route": map[string]any{"eta_min": 999}}
	tamOK, _ := parley.VerifyAgreement(&agrTampered)
	fmt.Printf("tampered agreement detected: %v\n", !tamOK)

	banner("12) SUMMARY")
	fmt.Printf("canonical schema: 3 messages validated against schema v0\n")
	fmt.Printf("signatures:       %v, %v, %v (alg: %s)\n",
		r1["signature_ok"], r2["signature_ok"], r3["signature_ok"], m1.Signature.Alg)
	nl2 := findNL(r2["clauses"].([]map[string]any))
	nl3 := findNL(r3["clauses"].([]map[string]any))
	fmt.Printf("NL clause (propose): binding=%v (%s) — proposal did not list clauses\n", nl2["binding"], nl2["detail"])
	fmt.Printf("NL clause (accept):  binding=%v (%s) — explicit accept listed 'c3'\n", nl3["binding"], nl3["detail"])
	fmt.Printf("check clauses in the proposal: %v\n", checkIDs(r2["clauses"].([]map[string]any)))
	fmt.Printf("tamper detectado: %v\n", !r2b["signature_ok"].(bool))
	fmt.Printf("body contract: request ok=%v, dest-missing ok=%v, propose ok=%v\n",
		r1c["body_contract"].(map[string]any)["ok"], bcBad["ok"], r2c["body_contract"].(map[string]any)["ok"])
	fmt.Printf("supervisor: deadline+fallback with enforcement (escalation=%v)\n", len(del) == 1)
	fmt.Printf("ranking:      top=%s score=%.2f (clause weight decides)\n",
		rk.Top.Proposal.From.ID, rk.Top.Score)
	fmt.Printf("thread:       %s after %d acts (offers=%d cap=%d)\n",
		th.State, len(th.Trace), th.OfferCount, th.Cap)

	if r1["schema_ok"].(bool) && r2["signature_ok"].(bool) && r3["signature_ok"].(bool) &&
		!r2b["signature_ok"].(bool) && !bcBad["ok"].(bool) && len(del) == 1 &&
		rEsc.Status == transport.StatusAccepted &&
		rk.Top.Proposal.From.ID == "dispatcher-8" && th.State == "settled" &&
		resProd.Status == transport.StatusAccepted && okCard && prodAccepted &&
		agrOK2 && !tamOK {
		os.Exit(0)
	}
	os.Exit(1)
}

func findNL(clauses []map[string]any) map[string]any {
	for _, m := range clauses {
		if _, ok := m["nl"]; ok {
			return m
		}
	}
	return map[string]any{}
}

func checkIDs(clauses []map[string]any) []any {
	var out []any
	for _, m := range clauses {
		if _, ok := m["check"]; ok {
			out = append(out, []any{m["id"], m["ok"]})
		}
	}
	return out
}
