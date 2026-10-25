// PARLEY v0 (Go) — demo de negociação de ponta a ponta (multi-tenant, não confiáveis).
// Espelho de demo.py: valida as 3 mensagens, verifica assinaturas, contrato
// híbrido (check + nl) e prova tamper-proof.
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
		"examples/negociacao.parley",             // rodando da raiz do repositório
		"sdk/go/demo/examples/negociacao.parley", // rodando da raiz do repositório (fallback)
		"../../examples/negociacao.parley",       // rodando de sdk/go (go run ./demo)
		"../../../examples/negociacao.parley",    // último recurso: da raiz
	} {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	fmt.Fprintln(os.Stderr, "erro: examples/negociacao.parley não encontrado")
	os.Exit(1)
	return ""
}

func main() {
	src := readExample()
	doc, err := parley.ParseText(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sintaxe:", err)
		os.Exit(1)
	}
	msgs := parley.Finalize(doc)
	for i, m := range msgs {
		msgs[i] = parley.Sign(m, m.From.ID, m.Tenant)
	}
	m1, m2, m3 := msgs[0], msgs[1], msgs[2]

	banner("1) VALIDAÇÃO (mensagem 1: request do PLANNER)")
	r1 := parley.Validate(m1, nil, NOW)
	pretty(r1)

	banner("2) VALIDAÇÃO (mensagem 2: propose do DISPATCHER contra o contrato da msg 1)")
	r2 := parley.Validate(m2, m1, NOW)
	pretty(r2)

	banner("3) VALIDAÇÃO (mensagem 3: accept seletivo do PLANNER)")
	r3 := parley.Validate(m3, m1, NOW)
	pretty(r3)

	banner("4) TAMPER PROOF: mensagem 2 com corpo adulterado")
	bad := *m2
	bad.Body = map[string]any{}
	for k, v := range m2.Body {
		bad.Body[k] = v
	}
	bad.Body["eta_min"] = 240 // não viola 'route.eta_min <= 150' (caminho route.*)
	conf := 0.5
	bad.Confidence = &conf // viola 'confidence > 0.85'
	ok, why := parley.Verify(&bad)
	r2b := parley.Validate(&bad, m1, NOW)
	fmt.Printf("assinatura: %v (%s)\n", ok, why)
	pretty(r2b["clauses"])

	banner("6) CONTRATO DE CORPO: body validado contra a capability (route.plan)")
	// A capability declarada no .parley vira contrato de corpo: request ->
	// INPUTS (origin/dest obrigatórios), propose -> OUTPUTS (route).
	capRoute := doc.Capabilities["route.plan"]
	contracts := map[string]*parley.Capability{"route.plan": &capRoute}

	// 6a) request legítimo: body tem origin+dest -> contrato de input OK
	r1c := parley.ValidateWith(m1, nil, NOW, contracts)
	fmt.Printf("request  (inputs):  body_contract.ok=%v  (%v)\n",
		r1c["body_contract"].(map[string]any)["ok"],
		r1c["body_contract"].(map[string]any)["capability"])

	// 6b) request com 'dest' ausente: violação reportada (não rejeitada)
	badReq := *m1
	badReq.Body = map[string]any{"origin": "Lisboa"} // sem dest
	parley.AssignID(&badReq)
	badReq = *parley.Sign(&badReq, "planner-42", "acme")
	r1b := parley.ValidateWith(&badReq, nil, NOW, contracts)
	bcBad := r1b["body_contract"].(map[string]any)
	fmt.Printf("request  (dest ausente): ok=%v  violações=%v\n",
		bcBad["ok"], bcBad["violations"])

	// 6c) propose legítima: body tem 'route' -> contrato de output OK
	r2c := parley.ValidateWith(m2, m1, NOW, contracts)
	fmt.Printf("propose  (outputs):  body_contract.ok=%v\n",
		r2c["body_contract"].(map[string]any)["ok"])

	banner("7) SUPERVISOR: deadline + fallback com enforcement runtime")
	// m1 tem deadline (2026-10-22T15:00:00Z) + fallback (dispatcher-backup).
	// O Supervisor é o watcher que FAZ o fallback acontecer: sem ele o
	// deadline seria só um warning.
	sup := transport.NewSupervisor().WithClock(func() time.Time { return NOW })
	fbInbox := transport.NewInbox()
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *transport.Result {
		return fbInbox.Deliver(b)
	})
	m1raw, _ := json.Marshal(m1)
	dl, _ := sup.Track(m1raw)
	fmt.Printf("escrow: %s  deadline=%s  pending=%v\n", m1.ID, dl.Format("15:04:05"), sup.Pending())

	// 7a) primary responde a tempo -> escalada cancelada
	sup.Reply(m1.ID, m2.ID)
	fmt.Printf("primary respondeu (propose): pending=%v  escaladas=%d\n",
		sup.Pending(), len(sup.SettleLog()))

	// 7b) cenário de timeout: nova request, primary calado, deadline passa
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
	fmt.Printf("nova request %s (deadline +5min), primary calado...\n", req2.ID)

	// avança o relógio para depois do deadline
	sup.WithClock(func() time.Time { return NOW.Add(6 * time.Minute) })
	del := sup.Tick()
	if len(del) == 1 {
		fmt.Printf("escalada ao fallback: to=%s reason=%s  (inbox do fallback: %d msg)\n",
			del[0].To, del[0].Reason, fbInbox.Len())
	} else {
		fmt.Printf("AVISO: esperada 1 escalada, obtidas %d\n", len(del))
	}

	// 7c) o sender re-targeta + re-assina o envelope para o fallback
	esc := transport.DeliverFallback(req2)
	esc.To = parley.AgentRef{ID: "dispatcher-backup"}
	parley.AssignID(esc)
	esc = parley.Sign(esc, "planner-42", "acme")
	escraw, _ := json.Marshal(esc)
	rEsc := fbInbox.Deliver(escraw)
	fmt.Printf("escalada re-target+re-sign aceita na inbox do fallback: %s (%s)\n",
		rEsc.Status, rEsc.Reason)

	banner("8) RANKING POR WEIGHT: duas propostas concorrentes para o MESMO pedido")
	// A: conf 0.93, eta 142, NÃO aceita a cláusula NL -> penalizada (score 0)
	// B: conf 0.95, eta 100, ACEITA a NL (c3) -> satisfaz tudo (score 1.0)
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
	fmt.Printf("topo: %s (score %.2f) — aceita a cláusula NL (binding) e satisfaz as checks\n",
		rk.Top.Proposal.From.ID, rk.Top.Score)

	banner("9) THREAD: máquina de estados dos acts sobre a negociação")
	th, err := parley.NewThreadFrom(8, []*parley.Message{m1, m2, m3})
	if err != nil {
		fmt.Fprintln(os.Stderr, "thread:", err)
		os.Exit(1)
	}
	for _, s := range th.Trace {
		fmt.Printf("  %-8s %s  -> estado %s\n", s.Act, s.ID, s.State)
	}
	fmt.Printf("estado final: %s (accept encerrou; ofertas=%d cap=%d)\n",
		th.State, th.OfferCount, th.Cap)

	banner("10) RUNTIME DE PRODUÇÃO: chaves reais + registro + fallback completo")
	// Diferente do modo teste (chave derivada do id), aqui as chaves são
	// ed25519 REAIS em keystore, os cards publicam a pública no registro, e o
	// inbox do fallback confia no registro (Registry.VerifyMessageWithCard).
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
			fmt.Fprintln(os.Stderr, "registro:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("registro: %d cards publicados (chaves reais, verificados contra card.Pub)\n",
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
	fmt.Printf("inbox com trust do registro: %s (%s) | assinatura via card: ", resProd.Status, resProd.Reason)
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
			fmt.Printf("escalada automática: to=%s provenance=%d assinatura re-verificada no card=%v (%s)\n",
				em.To.ID, len(em.Provenance), escOK, escWhy)
		}
	} else {
		fmt.Println("AVISO: escalada de produção não confirmada")
	}

	banner("11) AGREEMENT: recibo canônico da negociação, assinado pelos dois lados")
	// O accept da demo materializa o acordo: quem venceu, cláusulas NL que
	// viraram binding e o corpo aceito — auditável sem replayer a thread.
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
	fmt.Printf("acordo id=%s act=%s provider=%s binding=%v assinaturas=%d single=%v (%s) dupla=%v (%s)\n",
		agr.ID, agr.Act, agr.Provider.ID, agr.BindingClauses, len(agr.Signatures),
		agrOK1, agrWhy1, agrOK2, agrWhy2)

	// adulterar o corpo aceito invalida o id de conteúdo
	agrTampered := *agr
	agrTampered.Body = map[string]any{"route": map[string]any{"eta_min": 999}}
	tamOK, _ := parley.VerifyAgreement(&agrTampered)
	fmt.Printf("acordo adulterado detectado: %v\n", !tamOK)

	banner("12) RESUMO")
	fmt.Printf("schema canônico:  3 mensagens validadas contra schema v0\n")
	fmt.Printf("assinaturas:      %v, %v, %v (alg: %s)\n",
		r1["signature_ok"], r2["signature_ok"], r3["signature_ok"], m1.Signature.Alg)
	nl2 := findNL(r2["clauses"].([]map[string]any))
	nl3 := findNL(r3["clauses"].([]map[string]any))
	fmt.Printf("cláusula NL (propose): binding=%v (%s) — proposta não listou clauses\n", nl2["binding"], nl2["detail"])
	fmt.Printf("cláusula NL (accept):  binding=%v (%s) — aceite explícito listou 'c3'\n", nl3["binding"], nl3["detail"])
	fmt.Printf("cláusulas check na proposta: %v\n", checkIDs(r2["clauses"].([]map[string]any)))
	fmt.Printf("tamper detectado: %v\n", !r2b["signature_ok"].(bool))
	fmt.Printf("contrato de corpo: request ok=%v, dest-ausente ok=%v, propose ok=%v\n",
		r1c["body_contract"].(map[string]any)["ok"], bcBad["ok"], r2c["body_contract"].(map[string]any)["ok"])
	fmt.Printf("supervisor: deadline+fallback com enforcement (escalada=%v)\n", len(del) == 1)
	fmt.Printf("ranking:      topo=%s score=%.2f (weight das cláusulas decide)\n",
		rk.Top.Proposal.From.ID, rk.Top.Score)
	fmt.Printf("thread:       %s após %d atos (ofertas=%d cap=%d)\n",
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
