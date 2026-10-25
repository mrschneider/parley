package parley

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestInteropRankingGoMatchesPython: o MESMO conjunto de mensagens
// (fixture gerada pelo Python) deve produzir o MESMO ranking (ordem e
// scores) no Go e no Python — o weight-ranking é parte do protocolo,
// não do SDK.
func TestInteropRankingGoMatchesPython(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "interop-ranking.json"))
	if err != nil {
		t.Skip("fixture interop-ranking ausente")
	}
	var fx struct {
		Parent    map[string]any   `json:"parent"`
		Proposals []map[string]any `json:"proposals"`
		Expected  []struct {
			ID    string  `json:"id"`
			Score float64 `json:"score"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	var parent Message
	pb, _ := json.Marshal(fx.Parent)
	json.Unmarshal(pb, &parent)
	var proposals []*Message
	for _, p := range fx.Proposals {
		var m Message
		bm, _ := json.Marshal(p)
		json.Unmarshal(bm, &m)
		proposals = append(proposals, &m)
	}
	r := RankProposals(&parent, proposals)
	if len(r.Entries) != len(fx.Expected) {
		t.Fatalf("entradas = %d, esperado %d", len(r.Entries), len(fx.Expected))
	}
	for i, ex := range fx.Expected {
		got := r.Entries[i]
		if got.Proposal.ID != ex.ID {
			t.Fatalf("ordem[%d] = %s, esperado %s", i, got.Proposal.ID, ex.ID)
		}
		if got.Score != ex.Score {
			t.Fatalf("score[%d] = %v, esperado %v", i, got.Score, ex.Score)
		}
	}
}
