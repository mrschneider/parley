package parley

import (
	"testing"
	"time"
)

// threadMsg builds a minimal signed-free message for thread tests
// (threads are grammar, not trust — no signature needed).
func threadMsg(t *testing.T, from, to, act, target, parent string, ts time.Time) *Message {
	t.Helper()
	m := NewMessage(from, to, ts)
	m.Tenant = "acme"
	m.Act = act
	m.Target = target
	m.Parent = parent
	if target != "" {
		m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	}
	AssignID(m)
	return m
}

func TestThreadHappyPath(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)

	req := threadMsg(t, "planner-42", "dispatcher-7", "request", "route.plan", "", t0)
	if err := th.Apply(req); err != nil {
		t.Fatal(err)
	}
	if th.State != StateNegotiating || th.ID != req.ID || th.Target != "route.plan" {
		t.Fatalf("open errado: %+v", th)
	}

	prop := threadMsg(t, "dispatcher-7", "planner-42", "propose", "route.plan", req.ID, t0.Add(time.Minute))
	if err := th.Apply(prop); err != nil {
		t.Fatal(err)
	}
	if th.LastOffer != prop.ID || th.OfferCount != 1 {
		t.Fatalf("offer errado: last=%s count=%d", th.LastOffer, th.OfferCount)
	}

	cntr := threadMsg(t, "planner-42", "dispatcher-7", "counter", "route.plan", prop.ID, t0.Add(2*time.Minute))
	if err := th.Apply(cntr); err != nil {
		t.Fatal(err)
	}
	if th.OfferCount != 2 {
		t.Fatalf("counter deveria contar como oferta: %d", th.OfferCount)
	}

	acc := threadMsg(t, "dispatcher-7", "planner-42", "accept", "route.plan", cntr.ID, t0.Add(3*time.Minute))
	if err := th.Apply(acc); err != nil {
		t.Fatal(err)
	}
	if th.State != StateSettled {
		t.Fatalf("após accept: %s", th.State)
	}

	// thread encerrada: qualquer ato seguinte é inválido
	late := threadMsg(t, "planner-42", "dispatcher-7", "propose", "route.plan", acc.ID, t0.Add(4*time.Minute))
	if err := th.Apply(late); err == nil {
		t.Fatal("ato após settle deveria ser inválido")
	}
	if len(th.Trace) != 4 {
		t.Fatalf("trace = %d passos, esperado 4 (o inválido não entra)", len(th.Trace))
	}
}

func TestThreadOpenRequiresRequest(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)

	// inform não abre thread
	inf := threadMsg(t, "a", "b", "inform", "", "", t0)
	if err := th.Apply(inf); err == nil {
		t.Fatal("inform não deveria abrir thread")
	}
	if th.State != StateNew {
		t.Fatalf("estado após inform = %s, esperado new", th.State)
	}
	// request SEM target não abre
	req := threadMsg(t, "a", "b", "request", "", "", t0)
	if err := th.Apply(req); err == nil {
		t.Fatal("request sem target não deveria abrir thread")
	}
	// propose não abre
	pro := threadMsg(t, "a", "b", "propose", "route.plan", "", t0)
	if err := th.Apply(pro); err == nil {
		t.Fatal("propose não deveria abrir thread")
	}
}

func TestThreadCounterWithoutOffer(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	if err := th.Apply(req); err != nil {
		t.Fatal(err)
	}
	// counter direto, sem proposta anterior
	cn := threadMsg(t, "b", "a", "counter", "route.plan", req.ID, t0.Add(time.Minute))
	if err := th.Apply(cn); err == nil {
		t.Fatal("counter sem oferta anterior deveria ser inválido")
	}
}

func TestThreadRejectEnds(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	if err := th.Apply(req); err != nil {
		t.Fatal(err)
	}
	rej := threadMsg(t, "b", "a", "reject", "route.plan", req.ID, t0.Add(time.Minute))
	if err := th.Apply(rej); err != nil {
		t.Fatal(err)
	}
	if th.State != StateRejected {
		t.Fatalf("após reject: %s", th.State)
	}
	// não há "rejeito e depois proponho de novo"
	prop := threadMsg(t, "b", "a", "propose", "route.plan", rej.ID, t0.Add(2*time.Minute))
	if err := th.Apply(prop); err == nil {
		t.Fatal("propose após reject deveria ser inválido")
	}
}

func TestThreadCancelEnds(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	_ = th.Apply(req)
	prop := threadMsg(t, "b", "a", "propose", "route.plan", req.ID, t0.Add(time.Minute))
	_ = th.Apply(prop)
	can := threadMsg(t, "a", "b", "cancel", "route.plan", prop.ID, t0.Add(2*time.Minute))
	if err := th.Apply(can); err != nil {
		t.Fatal(err)
	}
	if th.State != StateRejected {
		t.Fatalf("após cancel: %s (cancel encerra sem acordo)", th.State)
	}
}

func TestThreadDelegateKeepsGoing(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	_ = th.Apply(req)
	del := threadMsg(t, "b", "c", "delegate", "route.plan", req.ID, t0.Add(time.Minute))
	if err := th.Apply(del); err != nil {
		t.Fatal(err)
	}
	if th.State != StateNegotiating {
		t.Fatalf("delegate não muda o estado: %s", th.State)
	}
	// o reencaminhado pode responder
	prop := threadMsg(t, "c", "a", "propose", "route.plan", del.ID, t0.Add(2*time.Minute))
	if err := th.Apply(prop); err != nil {
		t.Fatal(err)
	}
	if th.LastOffer != prop.ID {
		t.Fatal("oferta do reencaminhado não registrada")
	}
}

func TestThreadLateralActs(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	_ = th.Apply(req)
	inf := threadMsg(t, "b", "a", "inform", "route.plan", req.ID, t0.Add(time.Minute))
	if err := th.Apply(inf); err != nil {
		t.Fatal(err)
	}
	if th.State != StateNegotiating {
		t.Fatalf("inform lateral mudou o estado: %s", th.State)
	}
}

func TestThreadLoopCap(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(2) // teto de 2 ofertas
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	_ = th.Apply(req)
	last := req.ID
	for i := 1; i <= 2; i++ {
		act := "propose"
		if i > 1 {
			act = "counter"
		}
		om := threadMsg(t, "b", "a", act, "route.plan", last, t0.Add(time.Duration(i)*time.Minute))
		if err := th.Apply(om); err != nil {
			t.Fatalf("oferta %d: %v", i, err)
		}
		last = om.ID
	}
	// 3ª oferta excede o teto
	over := threadMsg(t, "b", "a", "counter", "route.plan", last, t0.Add(3*time.Minute))
	if err := th.Apply(over); err == nil {
		t.Fatal("oferta além do teto deveria ser inválida")
	}
	if th.OfferCount != 2 {
		t.Fatalf("offerCount = %d, esperado 2 (a inválida não conta)", th.OfferCount)
	}
}

func TestThreadFromMessages(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	prop := threadMsg(t, "b", "a", "propose", "route.plan", req.ID, t0.Add(time.Minute))
	acc := threadMsg(t, "a", "b", "accept", "route.plan", prop.ID, t0.Add(2*time.Minute))
	th, err := NewThreadFrom(0, []*Message{req, prop, acc})
	if err != nil {
		t.Fatal(err)
	}
	if th.State != StateSettled || len(th.Trace) != 3 {
		t.Fatalf("thread: state=%s trace=%d", th.State, len(th.Trace))
	}
	// sequência inválida: a PRIMEIRA violação é reportada
	bad := []*Message{req, acc, prop} // accept antes de qualquer oferta? — gramaticalmente
	// accept direto após request é válido (aceitar o pedido sem proposta).
	// Para inválida: counter sem oferta.
	bad2 := []*Message{req, threadMsg(t, "b", "a", "counter", "route.plan", req.ID, t0.Add(time.Minute))}
	if _, err := NewThreadFrom(0, bad2); err == nil {
		t.Fatal("counter sem oferta deveria falhar em NewThreadFrom")
	}
	_ = bad
}

func TestThreadTraceDeterministic(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	prop := threadMsg(t, "b", "a", "propose", "route.plan", req.ID, t0.Add(time.Minute))
	th, _ := NewThreadFrom(0, []*Message{req, prop})
	if th.Trace[0].Act != "request" || th.Trace[0].State != "negotiating" {
		t.Fatalf("trace[0] errado: %+v", th.Trace[0])
	}
	if th.Trace[1].Act != "propose" || th.Trace[1].State != "negotiating" {
		t.Fatalf("trace[1] errado: %+v", th.Trace[1])
	}
}
