package transport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	parley "example.org/parley-go"
)

var t0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// buildTracked builds a signed message with deadline + fallback (the shape
// a sender puts in escrow).
func buildTracked(t *testing.T, from, to, fb string, deadline string) *parley.Message {
	t.Helper()
	m := parley.NewMessage(from, to, t0)
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	m.Deadline = deadline
	m.Fallback = &parley.Fallback{To: parley.AgentRef{ID: fb}, Reason: "timeout"}
	parley.AssignID(m)
	return parley.Sign(m, from, "acme")
}

func buildReply(t *testing.T, from, to, act, parentID string) *parley.Message {
	t.Helper()
	m := parley.NewMessage(from, to, t0.Add(30*time.Second))
	m.Tenant = "acme"
	m.Act = act
	m.Parent = parentID
	m.Body = map[string]any{"route": map[string]any{"eta_min": 142}}
	parley.AssignID(m)
	return parley.Sign(m, from, "acme")
}

func raw(t *testing.T, m *parley.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSupervisorDeadlineEscalation(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	fbInbox := NewInbox()
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		return fbInbox.Deliver(b)
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	rawB := raw(t, req)
	dl, err := sup.Track(rawB)
	if err != nil {
		t.Fatal(err)
	}
	if !dl.Equal(time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC)) {
		t.Fatalf("deadline = %v", dl)
	}
	if len(sup.Pending()) != 1 {
		t.Fatalf("Pending = %v, esperado 1", sup.Pending())
	}

	// antes do deadline: nada
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("escalou antes do deadline: %v", d)
	}
	// no deadline: escalada ao fallback
	now := t0.Add(5 * time.Minute)
	sup.WithClock(func() time.Time { return now })
	del := sup.Tick()
	if len(del) != 1 {
		t.Fatalf("esperava 1 escalada no deadline, obtive %v", del)
	}
	if del[0].Reason != "deadline" || del[0].To != "dispatcher-backup" {
		t.Fatalf("escalada errada: %+v", del[0])
	}
	if len(sup.Pending()) != 0 {
		t.Fatalf("escrow não liberado após escalada: %v", sup.Pending())
	}
	// a inbox do fallback recebeu o envelope original (assinado pelo sender)
	if fbInbox.Len() != 1 {
		t.Fatalf("inbox do fallback = %d, esperado 1 (envelope original entregue)", fbInbox.Len())
	}
	// idempotência: tick de novo não re-escalada
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("re-escalada após deadline: %v", d)
	}
}

func TestSupervisorDeclineEscalation(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	var got []string
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		got = append(got, to)
		return &Result{Status: StatusAccepted}
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:30:00Z")
	if _, err := sup.Track(raw(t, req)); err != nil {
		t.Fatal(err)
	}
	// primary rejeita explicitamente, MUITO antes do deadline
	rej := buildReply(t, "dispatcher-7", "planner-42", "reject", req.ID)
	sup.Decline(req.ID, rej.ID)
	if len(got) != 1 || got[0] != "dispatcher-backup" {
		t.Fatalf("decline não escalou imediatamente: %v", got)
	}
	log := sup.SettleLog()
	if len(log) != 1 || log[0].Reason != "decline" || log[0].ReplyID != rej.ID {
		t.Fatalf("log de escalada errado: %+v", log)
	}
	// deadline passa depois: nada (já liquidado)
	sup.WithClock(func() time.Time { return t0.Add(31 * time.Minute) })
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("re-escalada após decline: %v", d)
	}
}

func TestSupervisorReplyCancels(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	calls := 0
	sup.RegisterSink("dispatcher-backup", func(string, []byte) *Result {
		calls++
		return &Result{Status: StatusAccepted}
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	if _, err := sup.Track(raw(t, req)); err != nil {
		t.Fatal(err)
	}
	// primary responde (propose) antes do deadline -> cancela a escalada
	prop := buildReply(t, "dispatcher-7", "planner-42", "propose", req.ID)
	sup.Reply(req.ID, prop.ID)
	// deadline passa: sem escalada
	sup.WithClock(func() time.Time { return t0.Add(6 * time.Minute) })
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("escalada apesar da resposta: %v", d)
	}
	if calls != 0 {
		t.Fatalf("fallback chamado %d vezes após reply", calls)
	}
	// reject tardio após reply: no-op
	sup.Decline(req.ID, "msg:late")
	if calls != 0 {
		t.Fatal("reject tardio após reply não deveria escalar")
	}
}

func TestSupervisorWire(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	var escalated []string
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		escalated = append(escalated, to)
		return &Result{Status: StatusAccepted}
	})

	// Wire: mensagem com deadline+fallback entra em escrow
	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	sup.Wire(raw(t, req))
	if len(sup.Pending()) != 1 {
		t.Fatalf("Wire não colocou em escrow: %v", sup.Pending())
	}

	// Wire: reply do primary cancela
	prop := buildReply(t, "dispatcher-7", "planner-42", "propose", req.ID)
	sup.Wire(raw(t, prop))
	if len(sup.Pending()) != 0 {
		t.Fatalf("Wire do reply não cancelou escrow: %v", sup.Pending())
	}

	// Wire: reject do primary escala
	req2 := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:40:00Z")
	sup.Wire(raw(t, req2))
	rej := buildReply(t, "dispatcher-7", "planner-42", "reject", req2.ID)
	sup.Wire(raw(t, rej))
	if len(escalated) != 1 {
		t.Fatalf("Wire do reject não escalou: %v", escalated)
	}
}

func TestSupervisorNoFallbackNoDeadline(t *testing.T) {
	sup := NewSupervisor()
	// sem deadline: não rastreável
	m := buildTracked(t, "a", "b", "c", "")
	m.Deadline = ""
	parley.AssignID(m)
	m = parley.Sign(m, "a", "acme")
	if _, err := sup.Track(raw(t, m)); err == nil {
		t.Fatal("mensagem sem deadline deveria rejeitar Track")
	}
	// sem fallback: rastreável, mas escalada é no-op
	m2 := buildTracked(t, "a", "b", "c", "2026-10-23T09:01:00Z")
	m2.Fallback = nil
	parley.AssignID(m2)
	m2 = parley.Sign(m2, "a", "acme")
	if _, err := sup.Track(raw(t, m2)); err != nil {
		t.Fatal(err)
	}
	sup.WithClock(func() time.Time { return t0.Add(2 * time.Minute) })
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("sem fallback não há escalada: %v", d)
	}
}

func TestDeliverFallbackRetarget(t *testing.T) {
	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	out := DeliverFallback(req)
	// envelope original é imutável
	if req.Fallback == nil || req.To.ID != "dispatcher-7" {
		t.Fatal("DeliverFallback mutou o original")
	}
	// fallback consumido, provenance estendida
	if out.Fallback != nil {
		t.Fatal("campo fallback deveria ser consumido no envelope de escalada")
	}
	if len(out.Provenance) != 1 || out.Provenance[0] != req.ID {
		t.Fatalf("provenance da escalada errada: %v", out.Provenance)
	}
	// id de conteúdo invalidado (envelope mudou) — o chamador faz AssignID
	// antes de re-assinar; o id antigo seria um mismatch de conteúdo
	if out.ID != "" {
		t.Fatal("escalada deveria invalidar o id de conteúdo (novo envelope)")
	}
}

func TestSupervisorEndToEndThroughInbox(t *testing.T) {
	// Cenário completo: sender faz request com deadline+fallback; o primary
	// nunca responde; o supervisor escalar e o envelope (re-signado pelo
	// sender, re-alvoçado) chega à inbox do fallback como mensagem NOVA
	// (aceita, não replay).
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	fbInbox := NewInbox()
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		return fbInbox.Deliver(b)
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	if _, err := sup.Track(raw(t, req)); err != nil {
		t.Fatal(err)
	}
	sup.WithClock(func() time.Time { return t0.Add(5 * time.Minute) })
	del := sup.Tick()
	if len(del) != 1 {
		t.Fatalf("escalada não disparou: %v", del)
	}
	// a escalada entrega o envelope ORIGINAL (assinado por planner-42):
	// verificação de assinatura passa na inbox do fallback
	res := del[0]
	if !strings.Contains(string(res.Delivered), "dispatcher-backup") {
		t.Fatal("envelope escalado deveria ser o original (com fallback field)")
	}
	if fbInbox.Len() != 1 {
		t.Fatalf("inbox do fallback deveria ter 1 mensagem, tem %d", fbInbox.Len())
	}
	// e o re-target + re-sign do sender produz uma mensagem nova e verificável
	esc := DeliverFallback(req)
	esc.To = parley.AgentRef{ID: "dispatcher-backup"}
	parley.AssignID(esc) // novo envelope -> novo id de conteúdo
	esc = parley.Sign(esc, "planner-42", "acme")
	r := fbInbox.Deliver(raw(t, esc))
	if r.Status != StatusAccepted {
		t.Fatalf("escalada re-target+re-sign deveria ser aceita: %s (%s)", r.Status, r.Reason)
	}
	if fbInbox.Len() != 2 {
		t.Fatalf("inbox do fallback = %d, esperado 2", fbInbox.Len())
	}
}
