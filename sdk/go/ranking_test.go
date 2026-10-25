package parley

import (
	"testing"
	"time"
)

var rankT0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// rankParent builds a request with a weighted contract:
// c1 check confidence>0.85 (w1.0), c2 check route.eta_min<=150 (w0.9),
// c3 nl (w0.9). Total = 2.8.
func rankParent() *Message {
	m := NewMessage("planner-42", "dispatcher-7", rankT0)
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	m.Expect = []Clause{
		{ID: "c1", Check: &Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0},
		{ID: "c2", Check: &Check{Field: "route.eta_min", Op: "lte", Value: 150}, Weight: 0.9},
		{ID: "c3", NL: "avoid roadworks in the next 24h", Weight: 0.9},
	}
	AssignID(m)
	return m
}

// rankProposal builds a propose replying to parent with the given body,
// confidence and explicitly-accepted clause ids.
func rankProposal(parent *Message, from string, body map[string]any, conf float64, clauses []string) *Message {
	m := NewMessage(from, parent.From.ID, rankT0.Add(time.Minute))
	m.Tenant = "acme"
	m.Act = "propose"
	m.Parent = parent.ID
	m.Body = body
	if conf > 0 {
		c := conf
		m.Confidence = &c
	}
	m.Clauses = clauses
	AssignID(m)
	return m
}

func TestRankingOrder(t *testing.T) {
	parent := rankParent()
	// A: satisfies c1 (conf 0.93) and c2 (eta 142), does NOT accept c3 (nl)
	//    -> score 0 (violated the unaccepted nl, weight 0.9)
	a := rankProposal(parent, "dispatcher-7",
		map[string]any{"route": map[string]any{"eta_min": 142}}, 0.93, nil)
	// B: satisfies c1 (conf 0.95) and c2 (eta 100) AND accepts c3 -> score 1.0
	b := rankProposal(parent, "dispatcher-7",
		map[string]any{"route": map[string]any{"eta_min": 100}}, 0.95, []string{"c3"})
	// C: viola c2 (eta 200) -> score 0
	c := rankProposal(parent, "dispatcher-7",
		map[string]any{"route": map[string]any{"eta_min": 200}}, 0.99, []string{"c3"})

	r := RankProposals(parent, []*Message{a, b, c})
	if len(r.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(r.Entries))
	}
	if r.Total != 2.8 {
		t.Fatalf("weight total = %v, expected 2.8", r.Total)
	}
	// B must be at the top (score 1.0)
	if r.Top == nil || r.Top.Proposal.ID != b.ID {
		t.Fatalf("top = %v, expected %s", r.Top, b.ID)
	}
	if r.Top.Score != 1.0 {
		t.Fatalf("top score = %v, expected 1.0", r.Top.Score)
	}
	// A and C have score 0; A comes before C by confidence (0.93 < 0.99? no —
	// C has conf 0.99, so C comes before A in the confidence tie-break)
	// Only checks that B is the top and that A and C have score 0.
	for _, e := range r.Entries {
		if e.Proposal.ID == a.ID && e.Score != 0 {
			t.Errorf("A should have score 0 (nl not accepted), got %v", e.Score)
		}
		if e.Proposal.ID == c.ID && e.Score != 0 {
			t.Errorf("C should have score 0 (c2 violated), got %v", e.Score)
		}
	}
}

func TestRankingWeightsDriveOrder(t *testing.T) {
	// Two offers: one satisfies the HEAVIEST clause and violates the lighter;
	// the other satisfies the lighter and violates the heaviest. Both have
	// "1 ok, 1 violated" -> score 0 for both. The tie-break is confidence.
	parent := rankParent()
	// P: conf 0.99 (satisfies c1), eta 200 (violates c2), accepts c3 -> c2 violated -> 0
	p := rankProposal(parent, "d", map[string]any{"route": map[string]any{"eta_min": 200}}, 0.99, []string{"c3"})
	// Q: conf 0.90 (satisfies c1), eta 100 (satisfies c2), does NOT accept c3 -> nl violated -> 0
	q := rankProposal(parent, "d", map[string]any{"route": map[string]any{"eta_min": 100}}, 0.90, nil)
	r := RankProposals(parent, []*Message{p, q})
	if len(r.Entries) != 2 {
		t.Fatalf("expected 2, got %d", len(r.Entries))
	}
	// both score 0; P (conf 0.99) must come before Q (conf 0.90)
	if r.Entries[0].Proposal.ID != p.ID {
		t.Fatalf("confidence tie-break: expected P at the top, got %s", r.Entries[0].Proposal.ID)
	}
}

func TestRankingFractional(t *testing.T) {
	// One offer satisfies only c1 (conf ok) and accepts c3, but violates c2 -> score 0.
	// Another satisfies c1 and c2, accepts c3 -> 1.0.
	// For a partial fraction I need an offer that satisfies a subset
	// WITHOUT violating any: but the rule is score 0 if there IS a violation.
	// So the only way to get a partial fraction is if EVERY clause it does
	// not satisfy is nl-NOT-accepted... which counts as a violation. Thus a
	// partial fraction only occurs when the offer satisfies a subset and the
	// rest do NOT exist (impossible) — or when weight<=0 (inactive).
	// Test: an inactive clause (weight 0) does not score.
	m := NewMessage("a", "b", rankT0)
	m.Act = "request"
	m.Expect = []Clause{
		{ID: "x", Check: &Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0},
		{ID: "y", NL: "inactive clause", Weight: 0}, // weight 0 -> inactive
	}
	AssignID(m)
	// offer satisfies x, ignores y (inactive) -> score 1.0 (total=1.0, satisfied=1.0)
	off := NewMessage("b", "a", rankT0.Add(time.Second))
	off.Act = "propose"
	off.Parent = m.ID
	conf := 0.9
	off.Confidence = &conf
	AssignID(off)
	r := RankProposals(m, []*Message{off})
	if r.Total != 1.0 {
		t.Fatalf("total = %v, expected 1.0 (inactive does not count)", r.Total)
	}
	if r.Top == nil {
		t.Fatal("Top is nil, expected the proposal")
	}
	if r.Top.Score != 1.0 {
		t.Fatalf("score = %v, expected 1.0 (inactive clause does not score)", r.Top.Score)
	}
}

func TestRankingNoOffers(t *testing.T) {
	parent := rankParent()
	r := RankProposals(parent, nil)
	if len(r.Entries) != 0 || r.Top != nil {
		t.Fatalf("no proposals: Top should be nil, got %+v", r.Top)
	}
}

func TestRankingIgnoresNonOffers(t *testing.T) {
	parent := rankParent()
	// an accept is not an offer -> ignored by the ranking
	acc := NewMessage("b", "a", rankT0.Add(time.Second))
	acc.Act = "accept"
	acc.Parent = parent.ID
	AssignID(acc)
	r := RankProposals(parent, []*Message{acc})
	if len(r.Entries) != 0 {
		t.Fatalf("accept must not enter the ranking, got %d", len(r.Entries))
	}
}
