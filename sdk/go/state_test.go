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
		t.Fatalf("counter should count as an offer: %d", th.OfferCount)
	}

	acc := threadMsg(t, "dispatcher-7", "planner-42", "accept", "route.plan", cntr.ID, t0.Add(3*time.Minute))
	if err := th.Apply(acc); err != nil {
		t.Fatal(err)
	}
	if th.State != StateSettled {
		t.Fatalf("after accept: %s", th.State)
	}

	// thread closed: any following act is invalid
	late := threadMsg(t, "planner-42", "dispatcher-7", "propose", "route.plan", acc.ID, t0.Add(4*time.Minute))
	if err := th.Apply(late); err == nil {
		t.Fatal("act after settle should be invalid")
	}
	if len(th.Trace) != 4 {
		t.Fatalf("trace = %d steps, expected 4 (the invalid one does not count)", len(th.Trace))
	}
}

func TestThreadOpenRequiresRequest(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)

	// inform does not open a thread
	inf := threadMsg(t, "a", "b", "inform", "", "", t0)
	if err := th.Apply(inf); err == nil {
		t.Fatal("inform should not open a thread")
	}
	if th.State != StateNew {
		t.Fatalf("state after inform = %s, expected new", th.State)
	}
	// request WITHOUT target does not open
	req := threadMsg(t, "a", "b", "request", "", "", t0)
	if err := th.Apply(req); err == nil {
		t.Fatal("request without target should not open a thread")
	}
	// propose does not open
	pro := threadMsg(t, "a", "b", "propose", "route.plan", "", t0)
	if err := th.Apply(pro); err == nil {
		t.Fatal("propose should not open a thread")
	}
}

func TestThreadCounterWithoutOffer(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(0)
	req := threadMsg(t, "a", "b", "request", "route.plan", "", t0)
	if err := th.Apply(req); err != nil {
		t.Fatal(err)
	}
	// direct counter, no previous offer
	cn := threadMsg(t, "b", "a", "counter", "route.plan", req.ID, t0.Add(time.Minute))
	if err := th.Apply(cn); err == nil {
		t.Fatal("counter without a previous offer should be invalid")
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
		t.Fatalf("after reject: %s", th.State)
	}
	// there is no "I reject and then propose again"
	prop := threadMsg(t, "b", "a", "propose", "route.plan", rej.ID, t0.Add(2*time.Minute))
	if err := th.Apply(prop); err == nil {
		t.Fatal("propose after reject should be invalid")
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
		t.Fatalf("after cancel: %s (cancel ends without agreement)", th.State)
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
		t.Fatalf("delegate does not change the state: %s", th.State)
	}
	// the delegate can reply
	prop := threadMsg(t, "c", "a", "propose", "route.plan", del.ID, t0.Add(2*time.Minute))
	if err := th.Apply(prop); err != nil {
		t.Fatal(err)
	}
	if th.LastOffer != prop.ID {
		t.Fatal("delegate's offer not recorded")
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
		t.Fatalf("lateral inform changed the state: %s", th.State)
	}
}

func TestThreadLoopCap(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	th := NewThread(2) // cap of 2 offers
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
			t.Fatalf("offer %d: %v", i, err)
		}
		last = om.ID
	}
	// 3rd offer exceeds the cap
	over := threadMsg(t, "b", "a", "counter", "route.plan", last, t0.Add(3*time.Minute))
	if err := th.Apply(over); err == nil {
		t.Fatal("offer beyond the cap should be invalid")
	}
	if th.OfferCount != 2 {
		t.Fatalf("offerCount = %d, expected 2 (the invalid one does not count)", th.OfferCount)
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
	// invalid sequence: the FIRST violation is reported
	bad := []*Message{req, acc, prop} // accept before any offer? — grammatically
	// a direct accept after request is valid (accepting the request without a proposal).
	// For an invalid one: counter without offer.
	bad2 := []*Message{req, threadMsg(t, "b", "a", "counter", "route.plan", req.ID, t0.Add(time.Minute))}
	if _, err := NewThreadFrom(0, bad2); err == nil {
		t.Fatal("counter without offer should fail in NewThreadFrom")
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
