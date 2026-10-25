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
		{ID: "c3", NL: "evitar obras nas próximas 24h", Weight: 0.9},
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
	// A: satisfaz c1 (conf 0.93) e c2 (eta 142), NÃO aceita c3 (nl)
	//    -> score 0 (violou a nl não aceita, peso 0.9)
	a := rankProposal(parent, "dispatcher-7",
		map[string]any{"route": map[string]any{"eta_min": 142}}, 0.93, nil)
	// B: satisfaz c1 (conf 0.95) e c2 (eta 100) E aceita c3 -> score 1.0
	b := rankProposal(parent, "dispatcher-7",
		map[string]any{"route": map[string]any{"eta_min": 100}}, 0.95, []string{"c3"})
	// C: viola c2 (eta 200) -> score 0
	c := rankProposal(parent, "dispatcher-7",
		map[string]any{"route": map[string]any{"eta_min": 200}}, 0.99, []string{"c3"})

	r := RankProposals(parent, []*Message{a, b, c})
	if len(r.Entries) != 3 {
		t.Fatalf("esperava 3 entradas, obtive %d", len(r.Entries))
	}
	if r.Total != 2.8 {
		t.Fatalf("total de peso = %v, esperado 2.8", r.Total)
	}
	// B deve estar no topo (score 1.0)
	if r.Top == nil || r.Top.Proposal.ID != b.ID {
		t.Fatalf("topo = %v, esperado %s", r.Top, b.ID)
	}
	if r.Top.Score != 1.0 {
		t.Fatalf("topo score = %v, esperado 1.0", r.Top.Score)
	}
	// A e C têm score 0; A vem antes de C por confidence (0.93 < 0.99? não —
	// C tem conf 0.99, então C vem antes de A em tie-break de confidence)
	// Verifica apenas que B é o topo e que A e C têm score 0.
	for _, e := range r.Entries {
		if e.Proposal.ID == a.ID && e.Score != 0 {
			t.Errorf("A deveria ter score 0 (nl não aceita), obtive %v", e.Score)
		}
		if e.Proposal.ID == c.ID && e.Score != 0 {
			t.Errorf("C deveria ter score 0 (c2 violada), obtive %v", e.Score)
		}
	}
}

func TestRankingWeightsDriveOrder(t *testing.T) {
	// Dois offers: um satisfaz a cláusula de MAIOR peso e viola a de menor;
	// o outro satisfaz a de menor e viola a de maior. Ambos têm "1 ok, 1
	// violada" -> score 0 nos dois. O tie-break é confidence.
	parent := rankParent()
	// P: conf 0.99 (satisfaz c1), eta 200 (viola c2), aceita c3 -> c2 viola -> 0
	p := rankProposal(parent, "d", map[string]any{"route": map[string]any{"eta_min": 200}}, 0.99, []string{"c3"})
	// Q: conf 0.90 (satisfaz c1), eta 100 (satisfaz c2), NÃO aceita c3 -> nl viola -> 0
	q := rankProposal(parent, "d", map[string]any{"route": map[string]any{"eta_min": 100}}, 0.90, nil)
	r := RankProposals(parent, []*Message{p, q})
	if len(r.Entries) != 2 {
		t.Fatalf("esperava 2, obtive %d", len(r.Entries))
	}
	// ambos score 0; P (conf 0.99) deve vir antes de Q (conf 0.90)
	if r.Entries[0].Proposal.ID != p.ID {
		t.Fatalf("tie-break por confidence: esperado P no topo, obtive %s", r.Entries[0].Proposal.ID)
	}
}

func TestRankingFractional(t *testing.T) {
	// Um offer satisfaz só c1 (conf ok) e aceita c3, mas viola c2 -> score 0.
	// Outro satisfaz c1 e c2, aceita c3 -> 1.0.
	// Para fração parcial, preciso de um offer que satisfaça um subconjunto
	// SEM violar nenhum: mas a regra é score 0 se HOUVER violação. Então a
	// única forma de fração parcial é se TODAS as cláusulas que ele não
	// satisfaz forem nl-NÃO-aceitas... que conta como violação. Assim,
	// fração parcial só ocorre quando o offer satisfaz um subconjunto e as
	// restantes NÃO existem (impossível) — ou quando weight<=0 (inativas).
	// Teste: cláusula inativa (weight 0) não pontua.
	m := NewMessage("a", "b", rankT0)
	m.Act = "request"
	m.Expect = []Clause{
		{ID: "x", Check: &Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0},
		{ID: "y", NL: "cláusula inativa", Weight: 0}, // weight 0 -> inativa
	}
	AssignID(m)
	// offer satisfaz x, ignora y (inativa) -> score 1.0 (total=1.0, satisfied=1.0)
	off := NewMessage("b", "a", rankT0.Add(time.Second))
	off.Act = "propose"
	off.Parent = m.ID
	conf := 0.9
	off.Confidence = &conf
	AssignID(off)
	r := RankProposals(m, []*Message{off})
	if r.Total != 1.0 {
		t.Fatalf("total = %v, esperado 1.0 (inativa não conta)", r.Total)
	}
	if r.Top == nil {
		t.Fatal("Top é nil, esperado a proposta")
	}
	if r.Top.Score != 1.0 {
		t.Fatalf("score = %v, esperado 1.0 (cláusula inativa não pontua)", r.Top.Score)
	}
}

func TestRankingNoOffers(t *testing.T) {
	parent := rankParent()
	r := RankProposals(parent, nil)
	if len(r.Entries) != 0 || r.Top != nil {
		t.Fatalf("sem propostas: Top deve ser nil, obtive %+v", r.Top)
	}
}

func TestRankingIgnoresNonOffers(t *testing.T) {
	parent := rankParent()
	// um accept não é oferta -> ignorado no ranking
	acc := NewMessage("b", "a", rankT0.Add(time.Second))
	acc.Act = "accept"
	acc.Parent = parent.ID
	AssignID(acc)
	r := RankProposals(parent, []*Message{acc})
	if len(r.Entries) != 0 {
		t.Fatalf("accept não deve entrar no ranking, obtive %d", len(r.Entries))
	}
}
