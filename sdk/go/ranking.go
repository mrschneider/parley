// PARLEY v0 — Ranking de propostas por weight das cláusulas.
//
// O schema declara clause.weight: "How much this clause matters to the
// sender (drives negotiation ranking)". Este arquivo implementa a promessa:
// dado um pedido (request) com cláusulas 'expect' ponderadas e as propostas
// recebidas (propose/counter), RankProposals devolve a ordenação das
// propostas pela fração de peso contratual que satisfazem.
//
// Regras (mesmas semânticas dos contratos híbridos — reportado, nunca
// rejeição dura; o ranking ORIENTA a escolha, o requestor decide):
//
//   - cláusula 'check' satisfeita:  +weight
//   - cláusula 'check' violada:     -weight  (contabilizada em violations)
//   - cláusula 'nl' sem aceite
//     explícito (id fora de m.Clauses): -weight  (não vinculante => custo)
//   - cláusula 'nl' com aceite explícito: +weight (binding => crédito)
//   - cláusulas com weight <= 0 não pontuam (inativas)
//   - score = peso satisfeito / peso total (0..1); quando há violação de
//     cláusula com peso positivo, score = 0 (penalidade dura: uma cláusula
//     importante violada não é compensada por outras satisfeitas)
//
// Ordem determinística: score descendente, confidence descendente, id
// ascendente (estável entre plataformas e execuções).
package parley

import "sort"

// RankEntry é uma proposta avaliada contra o contrato do pedido.
type RankEntry struct {
	Proposal   *Message         `json:"proposal"`
	Score      float64          `json:"score"`
	Satisfied  []string         `json:"satisfied,omitempty"`  // ids de cláusula creditadas
	Violations []string         `json:"violations,omitempty"` // ids de cláusula penalizadas
	Detail     []map[string]any `json:"detail"`               // avaliação cláusula a cláusula
}

// Ranking é o resultado da ordenação.
type Ranking struct {
	ParentID string      `json:"parent_id"`
	Total    float64     `json:"total"` // peso total das cláusulas ponderadas
	Entries  []RankEntry `json:"entries"`
	Top      *RankEntry  `json:"top,omitempty"` // nil quando não há propostas
}

// RankProposals ordena as propostas (mensagens com act propose/counter)
// contra as cláusulas 'expect' do pedido parent, usando clause.weight.
func RankProposals(parent *Message, proposals []*Message) *Ranking {
	r := &Ranking{Entries: []RankEntry{}}
	if parent == nil {
		return r
	}
	r.ParentID = parent.ID
	total := 0.0
	for _, c := range parent.Expect {
		if c.Weight > 0 {
			total += c.Weight
		}
	}
	r.Total = total

	for _, p := range proposals {
		if p == nil || !isOfferAct(p.Act) {
			continue
		}
		r.Entries = append(r.Entries, scoreProposal(parent, p, total))
	}
	sortRanking(r)
	return r
}

func isOfferAct(act string) bool {
	return act == "propose" || act == "counter"
}

func scoreProposal(parent, p *Message, total float64) RankEntry {
	e := RankEntry{Proposal: p, Satisfied: []string{}, Violations: []string{}, Detail: []map[string]any{}}
	satisfied := 0.0
	anyViolation := false
	for _, c := range parent.Expect {
		if c.Weight <= 0 {
			continue // cláusula inativa: não pontua
		}
		var ok bool
		var detail, kind string
		if c.NL != "" {
			kind = "nl"
			ok = acceptedID(p, c.ID)
			if ok {
				detail = "nl aceita explicitamente (binding)"
			} else {
				detail = "nl não aceita explicitamente -> não vinculante (penalidade)"
			}
		} else {
			kind = "check"
			ok, detail = CheckClause(c, p)
		}
		e.Detail = append(e.Detail, map[string]any{
			"id": c.ID, "kind": kind, "weight": c.Weight,
			"ok": ok, "detail": detail,
		})
		if ok {
			satisfied += c.Weight
			e.Satisfied = append(e.Satisfied, c.ID)
		} else {
			anyViolation = true
			e.Violations = append(e.Violations, c.ID)
		}
	}
	if total > 0 && !anyViolation {
		e.Score = satisfied / total
	}
	return e
}

func acceptedID(m *Message, id string) bool {
	for _, c := range m.Clauses {
		if c == id {
			return true
		}
	}
	return false
}

func sortRanking(r *Ranking) {
	sort.SliceStable(r.Entries, func(i, j int) bool {
		a, b := r.Entries[i], r.Entries[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		ca, cb := 0.0, 0.0
		if a.Proposal.Confidence != nil {
			ca = *a.Proposal.Confidence
		}
		if b.Proposal.Confidence != nil {
			cb = *b.Proposal.Confidence
		}
		if ca != cb {
			return ca > cb
		}
		return a.Proposal.ID < b.Proposal.ID
	})
	if len(r.Entries) > 0 {
		top := r.Entries[0]
		r.Top = &top
	}
}
