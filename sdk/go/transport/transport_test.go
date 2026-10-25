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
		t.Fatalf("status = %q (%s), expected %q", res.Status, res.Reason, want)
	}
	return res
}

func TestDeliverAccepted(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "",
		map[string]any{"eta_min": 35})
	expect(t, in, rawOf(t, m), StatusAccepted)
	if in.Len() != 1 {
		t.Fatalf("Len = %d, expected 1", in.Len())
	}
	got, ok := in.Get(m.ID)
	if !ok || got.Act != "inform" {
		t.Fatal("accepted message not retrievable by id")
	}
}

func TestDeliverReplayDuplicate(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "",
		map[string]any{"eta_min": 35})
	expect(t, in, rawOf(t, m), StatusAccepted)
	// same payload again (broker redelivered) -> duplicate, no effect
	res := expect(t, in, rawOf(t, m), StatusDuplicate)
	if !strings.Contains(res.Reason, "replay") {
		t.Fatalf("reason without replay: %q", res.Reason)
	}
	if in.Len() != 1 {
		t.Fatalf("Len = %d after replay, expected 1 (idempotency)", in.Len())
	}
}

func TestDeliverTamperedRejected(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "",
		map[string]any{"eta_min": 35})
	raw := rawOf(t, m)
	expect(t, in, raw, StatusAccepted)
	// tampering: same id, body changed after the signature
	tampered := m
	tampered.Body = map[string]any{"eta_min": 999}
	res := expect(t, in, rawOf(t, tampered), StatusRejected)
	if !strings.Contains(res.Reason, "signature") {
		t.Fatalf("reason without signature: %q", res.Reason)
	}
}

func TestDeliverUnsignedRejected(t *testing.T) {
	in := NewInbox()
	m := parley.NewMessage("planner-42", "dispatcher-7", time.Now().UTC())
	m.Tenant = "acme"
	m.Act = "inform"
	m.Body = map[string]any{"x": 1}
	parley.AssignID(m) // valid id, WITHOUT signature
	res := expect(t, in, rawOf(t, m), StatusRejected)
	if !strings.Contains(res.Reason, "signature") {
		t.Fatalf("reason without signature: %q", res.Reason)
	}
}

func TestDeliverSchemaRejected(t *testing.T) {
	in := NewInbox()
	m := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", nil)
	m.Act = "teleport" // act fora do enum do schema v0
	res := expect(t, in, rawOf(t, m), StatusRejected)
	if !strings.Contains(res.Reason, "schema") {
		t.Fatalf("reason without schema: %q", res.Reason)
	}
}

func TestDeliverBadJSONRejected(t *testing.T) {
	in := NewInbox()
	res := expect(t, in, []byte("{not json"), StatusRejected)
	if !strings.Contains(res.Reason, "json") {
		t.Fatalf("reason without json: %q", res.Reason)
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
		t.Fatalf("reason without tenant: %q", res.Reason)
	}
}

func TestContractReportedNotRejected(t *testing.T) {
	in := NewInbox()
	// parent: proposal with a machine clause (eta) + an NL clause
	prop := parley.NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	prop.Tenant = "acme"
	prop.Act = "propose"
	prop.Body = map[string]any{"eta_min": 40}
	prop.Expect = []parley.Clause{
		{ID: "c1", Check: &parley.Check{Field: "eta_min", Op: "lte", Value: 30}, Weight: 1.0},
		{ID: "c2", NL: "no traffic fines on the route", Weight: 0.8},
	}
	parley.AssignID(prop)
	prop = parley.Sign(prop, "planner-42", "acme")
	expect(t, in, rawOf(t, prop), StatusAccepted)

	// reply: explicitly accepts c2 (becomes binding) but violates c1 (eta 40 > 30)
	acc := parley.NewMessage("dispatcher-7", "planner-42", time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC))
	acc.Tenant = "acme"
	acc.Act = "accept"
	acc.Parent = prop.ID
	acc.Body = map[string]any{"eta_min": 40}
	acc.Clauses = []string{"c2"}
	parley.AssignID(acc)
	acc = parley.Sign(acc, "dispatcher-7", "acme")

	res := expect(t, in, rawOf(t, acc), StatusAccepted) // reported, not rejected
	if res.Validation == nil {
		t.Fatal("Validation missing from reply with known parent")
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
		t.Fatalf("c1 violation not reported in warnings: %v", res.Warnings)
	}
	if !foundBinding {
		t.Fatal("NL clause c2 not marked as binding after explicit accept")
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

	// "restart": new inbox, same state file
	in2 := NewInbox(WithStoreFile(path))
	if err := in2.Load(); err != nil {
		t.Fatal(err)
	}
	if in2.Len() != 2 {
		t.Fatalf("Len after Load = %d, expected 2", in2.Len())
	}
	// broker replay after the restart is still detected
	expect(t, in2, rawOf(t, m1), StatusDuplicate)
	// a new message is still accepted
	m3 := buildMsg(t, "planner-42", "dispatcher-7", "inform", "", map[string]any{"c": 3})
	expect(t, in2, rawOf(t, m3), StatusAccepted)
	if in2.Len() != 3 {
		t.Fatalf("Len = %d, expected 3", in2.Len())
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
		t.Fatalf("Len = %d after pump, expected 2 (replay deduped)", in.Len())
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
		t.Fatalf("status = %q, expected accepted", res.Status)
	}
	// replay via HTTP: 200 (idempotente) + duplicate
	res2, err := srv.Send(ts.URL, "planner", m)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != StatusDuplicate {
		t.Fatalf("status = %q, expected duplicate", res2.Status)
	}
	// tampered via HTTP: 422
	tampered := m
	tampered.Body = map[string]any{"eta_min": 1}
	res3, err := srv.Send(ts.URL, "planner", tampered)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Status != StatusRejected {
		t.Fatalf("status = %q, expected rejected", res3.Status)
	}
	// inbox inexistente: 404
	resp, err := http.Post(ts.URL+"/inboxes/none", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, expected 404", resp.StatusCode)
	}
	// health
	resp2, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("health = %d, expected 200", resp2.StatusCode)
	}
}

// TestWithVerifierOption ensures the injectable verifier replaces the
// default (e.g. a public key coming from the registry instead of derivation).
func TestWithVerifierOption(t *testing.T) {
	calls := 0
	in := NewInbox(WithVerifier(func(*parley.Message) (bool, string) {
		calls++
		return true, "ok (injetado)"
	}))
	// message without signature: the injected verifier lets it pass
	m := parley.NewMessage("a-1", "b-2", time.Now().UTC())
	m.Tenant = "acme"
	m.Act = "inform"
	parley.AssignID(m)
	expect(t, in, rawOf(t, m), StatusAccepted)
	if calls != 1 {
		t.Fatalf("injected verifier called %d times", calls)
	}
}
