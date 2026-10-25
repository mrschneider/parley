package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	parley "example.org/parley-go"
)

// buildMsg builds a signed, content-addressed message (the shape a sender
// produces before handing raw JSON to any transport).
func buildMsg(t *testing.T, from, to, act string, parent string, body map[string]any) *parley.Message {
	t.Helper()
	m := parley.NewMessage(from, to, time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = act
	m.Parent = parent
	m.Body = body
	parley.AssignID(m)
	return parley.Sign(m, from, "acme")
}

func rawOf(t *testing.T, m *parley.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func expect(t *testing.T, in *Inbox, raw []byte, want string) *Result {
	t.Helper()
	res := in.Deliver(raw)
	if res.Status != want {
		t.Fatalf("status = %q (%s), esperado %q", res.Status, res.Reason, want)
	}
	return res
}

func TestDeliverAccepted(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "",
		map[string]any{"eta_min": 35})
	expect(t, in, rawOf(t, m), StatusAccepted)
	if in.Len() != 1 {
		t.Fatalf("Len = %d, esperado 1", in.Len())
	}
	got, ok := in.Get(m.ID)
	if !ok || got.Act != "inform" {
		t.Fatal("mensagem aceita não recuperável por id")
	}
}

func TestDeliverReplayDuplicate(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "",
		map[string]any{"eta_min": 35})
	expect(t, in, rawOf(t, m), StatusAccepted)
	// mesmo payload de novo (broker redeliverou) -> duplicate, sem efeito
	res := expect(t, in, rawOf(t, m), StatusDuplicate)
	if !strings.Contains(res.Reason, "replay") {
		t.Fatalf("reason sem replay: %q", res.Reason)
	}
	if in.Len() != 1 {
		t.Fatalf("Len = %d após replay, esperado 1 (idempotência)", in.Len())
	}
}

func TestDeliverTamperedRejected(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "",
		map[string]any{"eta_min": 35})
	raw := rawOf(t, m)
	expect(t, in, raw, StatusAccepted)
	// adulteração: mesmo id, corpo alterado após a assinatura
	tampered := m
	tampered.Body = map[string]any{"eta_min": 999}
	res := expect(t, in, rawOf(t, tampered), StatusRejected)
	if !strings.Contains(res.Reason, "assinatura") {
		t.Fatalf("reason sem assinatura: %q", res.Reason)
	}
}

func TestDeliverUnsignedRejected(t *testing.T) {
	in := NewInbox()
	m := parley.NewMessage("planner-42", "dispatcher-7", time.Now().UTC())
	m.Tenant = "acme"
	m.Act = "inform"
	m.Body = map[string]any{"x": 1}
	parley.AssignID(m) // id válido, SEM assinatura
	res := expect(t, in, rawOf(t, m), StatusRejected)
	if !strings.Contains(res.Reason, "assinatura") {
		t.Fatalf("reason sem assinatura: %q", res.Reason)
	}
}

func TestDeliverSchemaRejected(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", nil)
	m.Act = "teleport" // act fora do enum do schema v0
	res := expect(t, in, rawOf(t, m), StatusRejected)
	if !strings.Contains(res.Reason, "schema") {
		t.Fatalf("reason sem schema: %q", res.Reason)
	}
}

func TestDeliverBadJSONRejected(t *testing.T) {
	in := NewInbox()
	res := expect(t, in, []byte("{não é json"), StatusRejected)
	if !strings.Contains(res.Reason, "json") {
		t.Fatalf("reason sem json: %q", res.Reason)
	}
}

func TestDeliverTenantFilter(t *testing.T) {
	in := NewInbox(WithTenantFilter("acme"))
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", nil)
	m.Tenant = "globex"
	parley.AssignID(m)
	m = parley.Sign(m, "planner-42", "globex")
	res := expect(t, in, rawOf(t, m), StatusRejected)
	if !strings.Contains(res.Reason, "tenant") {
		t.Fatalf("reason sem tenant: %q", res.Reason)
	}
}

func TestContractReportedNotRejected(t *testing.T) {
	in := NewInbox()
	// pai: proposta com cláusula máquina (eta) + cláusula NL
	prop := parley.NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	prop.Tenant = "acme"
	prop.Act = "propose"
	prop.Body = map[string]any{"eta_min": 40}
	prop.Expect = []parley.Clause{
		{ID: "c1", Check: &parley.Check{Field: "eta_min", Op: "lte", Value: 30}, Weight: 1.0},
		{ID: "c2", NL: "sem multas de trânsito na rota", Weight: 0.8},
	}
	parley.AssignID(prop)
	prop = parley.Sign(prop, "planner-42", "acme")
	expect(t, in, rawOf(t, prop), StatusAccepted)

	// resposta: aceita explicitamente o c2 (vira binding) mas viola o c1 (eta 40 > 30)
	acc := parley.NewMessage("dispatcher-7", "planner-42", time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC))
	acc.Tenant = "acme"
	acc.Act = "accept"
	acc.Parent = prop.ID
	acc.Body = map[string]any{"eta_min": 40}
	acc.Clauses = []string{"c2"}
	parley.AssignID(acc)
	acc = parley.Sign(acc, "dispatcher-7", "acme")

	res := expect(t, in, rawOf(t, acc), StatusAccepted) // reportado, não rejeitado
	if res.Validation == nil {
		t.Fatal("Validation ausente em resposta com pai conhecido")
	}
	var foundViolation, foundBinding bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "c1") {
			foundViolation = true
		}
	}
	clauses, _ := res.Validation["clauses"].([]map[string]any)
	for _, c := range clauses {
		if c["id"] == "c2" && c["binding"] == true {
			foundBinding = true
		}
	}
	if !foundViolation {
		t.Fatalf("violação de c1 não reportada em warnings: %v", res.Warnings)
	}
	if !foundBinding {
		t.Fatal("cláusula NL c2 não marcada como binding após aceite explícito")
	}
}

func TestStoreFileSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inbox.json")

	in1 := NewInbox(WithStoreFile(path))
	m1 := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", map[string]any{"a": 1})
	m2 := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", map[string]any{"b": 2})
	expect(t, in1, rawOf(t, m1), StatusAccepted)
	expect(t, in1, rawOf(t, m2), StatusAccepted)

	// "restart": novo inbox, mesmo arquivo de estado
	in2 := NewInbox(WithStoreFile(path))
	if err := in2.Load(); err != nil {
		t.Fatal(err)
	}
	if in2.Len() != 2 {
		t.Fatalf("Len após Load = %d, esperado 2", in2.Len())
	}
	// replay do broker após o restart continua detectado
	expect(t, in2, rawOf(t, m1), StatusDuplicate)
	// mensagem nova continua aceita
	m3 := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", map[string]any{"c": 3})
	expect(t, in2, rawOf(t, m3), StatusAccepted)
	if in2.Len() != 3 {
		t.Fatalf("Len = %d, esperado 3", in2.Len())
	}
}

func TestQueuePump(t *testing.T) {
	q := NewQueue(4)
	in := NewInbox()
	m1 := buildMsg(t, "a-1", "b-2", "inform", "", map[string]any{"n": 1})
	m2 := buildMsg(t, "a-1", "b-2", "inform", "", map[string]any{"n": 2})
	q.Send(rawOf(t, m1))
	q.Send(rawOf(t, m2))
	q.Send(rawOf(t, m1)) // replay enfileirado
	done := make(chan struct{})
	go func() { q.Pump(in); close(done) }()
	close(q.ch) // fecha: Pump drena e encerra
	<-done
	if in.Len() != 2 {
		t.Fatalf("Len = %d após pump, esperado 2 (replay dedupado)", in.Len())
	}
}

func TestHTTPRoundTrip(t *testing.T) {
	in := NewInbox()
	srv := NewHTTPServer()
	srv.RegisterInbox("planner", in)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", map[string]any{"eta_min": 35})
	res, err := srv.Send(ts.URL, "planner", m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAccepted {
		t.Fatalf("status = %q, esperado accepted", res.Status)
	}
	// replay via HTTP: 200 (idempotente) + duplicate
	res2, err := srv.Send(ts.URL, "planner", m)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StatusDuplicate {
		t.Fatalf("status = %q, esperado duplicate", res2.Status)
	}
	// adulterada via HTTP: 422
	tampered := m
	tampered.Body = map[string]any{"eta_min": 1}
	res3, err := srv.Send(ts.URL, "planner", tampered)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Status != StatusRejected {
		t.Fatalf("status = %q, esperado rejected", res3.Status)
	}
	// inbox inexistente: 404
	resp, err := http.Post(ts.URL+"/inboxes/nenhum", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, esperado 404", resp.StatusCode)
	}
	// health
	resp2, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, esperado 200", resp2.StatusCode)
	}
}

// TestWithVerifierOption garante que o verificador injetável substitui o
// padrão (ex.: chave pública vinda do registry em vez de derivação).
func TestWithVerifierOption(t *testing.T) {
	calls := 0
	in := NewInbox(WithVerifier(func(*parley.Message) (bool, string) {
		calls++
		return true, "ok (injetado)"
	}))
	// mensagem sem assinatura: o verificador injetado deixa passar
	m := parley.NewMessage("a-1", "b-2", time.Now().UTC())
	m.Tenant = "acme"
	m.Act = "inform"
	parley.AssignID(m)
	expect(t, in, rawOf(t, m), StatusAccepted)
	if calls != 1 {
		t.Fatalf("verificador injetado chamado %d vezes", calls)
	}
}
