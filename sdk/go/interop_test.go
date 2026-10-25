package parley

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInteropGoToPython: Go constrói a MESMA mensagem do fixture (ts fixo),
// assina, e escreve o JSON para o Python verificar. Também compara
// canonical bytes e id de conteúdo com o esperado do Python.
func TestInteropGoToPython(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "interop-id.json"))
	if err != nil {
		t.Skip("fixture ausente")
	}
	var fx struct {
		ExpectedID string `json:"expected_id"`
		CanonHex   string `json:"canon_hex"`
		Signed     map[string]any
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}

	// reconstrói no Go com os mesmos valores
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Intent = "rota rápida"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto", "n": int64(3), "pi": 3.5}
	conf := 0.9
	m.Confidence = &conf
	m.Expect = []Clause{{ID: "c1", Check: &Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0}}

	// 1) bytes canônicos idênticos ao Python
	canon, _ := CanonicalBytes(m)
	var hexs [1024]byte
	copy(hexs[:], canon)
	if canonHexStr := bytesToHex(canon); canonHexStr != fx.CanonHex {
		t.Errorf("canonical bytes divergem:\n go: %s\n py: %s", canonHexStr, fx.CanonHex)
	}
	_ = hexs
	// 2) id de conteúdo idêntico
	AssignID(m)
	if m.ID != fx.ExpectedID {
		t.Errorf("id de conteúdo diverge:\n go: %s\n py: %s", m.ID, fx.ExpectedID)
	}
	// 3) assina no Go e exporta para o Python verificar
	signed := Sign(m, "planner-42", "acme")
	var out map[string]any
	bm, _ := json.Marshal(signed)
	json.Unmarshal(bm, &out)
	os.WriteFile(filepath.Join("testdata", "interop-go.json"),
		mustMarshal(out), 0644)
	t.Logf("Go assinou %s; Python deve verificar", m.ID)
}

func bytesToHex(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = h[c>>4], h[c&0xf]
	}
	return string(out)
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
