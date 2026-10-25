// PARLEY v0 — Proposal ranking by clause weight.
//
// The schema declares clause.weight: "How much this clause matters to the
// sender (drives negotiation ranking)". This file implements that promise:
// given a request with weighted 'expect' clauses and the received proposals
// (propose/counter), RankProposals returns the ordering of the proposals
// by the fraction of contractual weight they satisfy.
//
// Rules (same semantics as the hybrid contracts — reported, never a hard
// rejection; the ranking GUIDES the choice, the requestor decides):
//
//   - 'check' clause satisfied:  +weight
//   - 'check' clause violated:   -weight  (counted in violations)
//   - 'nl' clause without explicit accept
//     (id not in m.Clauses): -weight  (non-binding => cost)
//   - 'nl' clause with explicit accept: +weight (binding => credit)
//   - clauses with weight <= 0 do not score (inactive)
//   - score = satisfied weight / total weight (0..1); when a positive-weight
//     clause is violated, score = 0 (hard penalty: one important violated
//     clause is not offset by other satisfied ones)
//
// Deterministic order: score descending, confidence descending, id
// ascending (stable across platforms and runs).
package parley

import "sort"

// RankEntry is one proposal evaluated against the request's contract.
type RankEntry struct {
	Proposal   *Message         `json:"proposal"`
	Score      float64          `json:"score"`
	Satisfied  []string         `json:"satisfied,omitempty"`  // credited clause ids
	Violations []string         `json:"violations,omitempty"` // penalized clause ids
	Detail     []map[string]any `json:"detail"`               // clause-by-clause evaluation
}

// Ranking is the result of the ordering.
type Ranking struct {
	ParentID string      `json:"parent_id"`
	Total    float64     `json:"total"` // total weight of the weighted clauses
	Entries  []RankEntry `json:"entries"`
	Top      *RankEntry  `json:"top,omitempty"` // nil when there are no proposals
}

// RankProposals orders the proposals (messages with act propose/counter)
// against the parent request's 'expect' clauses, using clause.weight.
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
			continue // inactive clause: no score
		}
		var ok bool
		var detail, kind string
		if c.NL != "" {
			kind = "nl"
			ok = acceptedID(p, c.ID)
			if ok {
				detail = "nl explicitly accepted (binding)"
			} else {
				detail = "nl not explicitly accepted -> non-binding (penalty)"
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
