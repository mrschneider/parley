package transport

import (
	"strings"
	"testing"
	"time"

	parley "example.org/parley-go"
)

// offerMsg builds a signed offer (propose/counter) with body, confidence
// and optionally accepted clause ids.
func offerMsg(t *testing.T, from, to, act, parentID string, body map[string]any,
	conf float64, clauses []string) *parley.Message {
	t.Helper()
	m := parley.NewMessage(from, to, t0.Add(30*time.Second))
	m.Tenant = "acme"
	m.Act = act
	m.Target = "route.plan"
	m.Parent = parentID
	m.Body = body
	if conf > 0 {
		c := conf
		m.Confidence = &c
	}
	m.Clauses = clauses
	parley.AssignID(m)
	return parley.Sign(m, from, "acme")
}

// rankRequest builds a signed request with a weighted contract
// (c1 confidence>0.85 w1.0, c2 route.eta_min<=150 w0.9, c3 nl w0.9).
func rankRequest(t *testing.T) *parley.Message {
	t.Helper()
	m := parley.NewMessage("planner-42", "dispatcher-7", t0)
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	m.Expect = []parley.Clause{
		{ID: "c1", Check: &parley.Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0},
		{ID: "c2", Check: &parley.Check{Field: "route.eta_min", Op: "lte", Value: 150}, Weight: 0.9},
		{ID: "c3", NL: "avoid roadworks in the next 24h", Weight: 0.9},
	}
	parley.AssignID(m)
	return parley.Sign(m, "planner-42", "acme")
}

func TestInboxThreadAndRanking(t *testing.T) {
	in := NewInbox()
	req := rankRequest(t)
	expect(t, in, rawOf(t, req), StatusAccepted)
	st, ok := in.ThreadState(req.ID)
	if !ok || st != parley.StateNegotiating {
		t.Fatalf("request thread: %q ok=%v", st, ok)
	}

	// offer A: conf 0.93, eta 142, does NOT accept the nl -> score 0
	a := offerMsg(t, "dispatcher-7", "planner-42", "propose", req.ID,
		map[string]any{"route": map[string]any{"eta_min": 142}}, 0.93, nil)
	resA := expect(t, in, rawOf(t, a), StatusAccepted)
	if resA.Validation == nil {
		t.Fatal("Validation missing from the offer (known parent)")
	}
	rkA, ok := resA.Validation["ranking"].(*parley.Ranking)
	if !ok {
		t.Fatalf("ranking missing from the offer: %v", resA.Validation)
	}
	if rkA.Top == nil || rkA.Top.Proposal.ID != a.ID {
		t.Fatalf("with 1 offer, it must be the top: %+v", rkA.Top)
	}
	if rkA.Top.Score != 0 {
		t.Fatalf("offer without accepting the nl should have score 0, got %v", rkA.Top.Score)
	}
	if st2, _ := in.ThreadState(req.ID); st2 != parley.StateNegotiating {
		t.Fatalf("after proposal: %q", st2)
	}

	// offer B: conf 0.95, eta 100, accepts the nl (c3) -> score 1.0
	b := offerMsg(t, "dispatcher-8", "planner-42", "propose", req.ID,
		map[string]any{"route": map[string]any{"eta_min": 100}}, 0.95, []string{"c3"})
	resB := expect(t, in, rawOf(t, b), StatusAccepted)
	rkB := resB.Validation["ranking"].(*parley.Ranking)
	if rkB.Top == nil || rkB.Top.Proposal.ID != b.ID {
		t.Fatalf("B (score 1.0) should be the top, got %+v", rkB.Top)
	}
	if len(rkB.Entries) != 2 {
		t.Fatalf("ranking should have 2 offers, got %d", len(rkB.Entries))
	}

	// Direct API: RankProposals(root) returns the same ordering
	rk := in.RankProposals(req.ID)
	if rk == nil || rk.Top == nil || rk.Top.Proposal.ID != b.ID {
		t.Fatalf("Inbox.RankProposals: topo errado: %+v", rk)
	}

	// accept encerra a thread
	acc := offerMsg(t, "planner-42", "dispatcher-7", "accept", req.ID,
		map[string]any{"route": map[string]any{"eta_min": 100}}, 0, []string{"c3"})
	expect(t, in, rawOf(t, acc), StatusAccepted)
	if st3, _ := in.ThreadState(req.ID); st3 != parley.StateSettled {
		t.Fatalf("after accept: %q", st3)
	}

	// act after terminal: reported (warning), not rejected
	late := offerMsg(t, "dispatcher-7", "planner-42", "propose", req.ID,
		map[string]any{"route": map[string]any{"eta_min": 99}}, 0.99, nil)
	resLate := expect(t, in, rawOf(t, late), StatusAccepted)
	found := false
	for _, w := range resLate.Warnings {
		if strings.Contains(w, "closed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("act after terminal should report a thread warning: %v", resLate.Warnings)
	}
}

func TestInboxThreadCounterWithoutOffer(t *testing.T) {
	in := NewInbox()
	req := rankRequest(t)
	expect(t, in, rawOf(t, req), StatusAccepted)
	// direct counter, no previous offer -> grammar warning
	cn := offerMsg(t, "dispatcher-7", "planner-42", "counter", req.ID,
		map[string]any{"route": map[string]any{"eta_min": 140}}, 0.9, nil)
	res := expect(t, in, rawOf(t, cn), StatusAccepted)
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "counter") {
			found = true
		}
	}
	if !found {
		t.Fatalf("counter without offer should report a warning: %v", res.Warnings)
	}
}
