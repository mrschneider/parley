package parley

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInteropGoToPython: Go builds the SAME message as the fixture (fixed
// ts), signs it, and writes the JSON for Python to verify. It also compares
// the canonical bytes and content id against Python's expected values.
func TestInteropGoToPython(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "interop-id.json"))
	if err != nil {
		t.Skip("fixture missing")
	}
	var fx struct {
		ExpectedID string `json:"expected_id"`
		CanonHex   string `json:"canon_hex"`
		Signed     map[string]any
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}

	// reconstructs it in Go with the same values
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Intent = "fast route"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto", "n": int64(3), "pi": 3.5}
	conf := 0.9
	m.Confidence = &conf
	m.Expect = []Clause{{ID: "c1", Check: &Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0}}

	// 1) canonical bytes identical to Python
	canon, _ := CanonicalBytes(m)
	var hexs [1024]byte
	copy(hexs[:], canon)
	if canonHexStr := bytesToHex(canon); canonHexStr != fx.CanonHex {
		t.Errorf("canonical bytes differ:\n go: %s\n py: %s", canonHexStr, fx.CanonHex)
	}
	_ = hexs
	// 2) identical content id
	AssignID(m)
	if m.ID != fx.ExpectedID {
		t.Errorf("content id differs:\n go: %s\n py: %s", m.ID, fx.ExpectedID)
	}
	// 3) signs in Go and exports for Python to verify
	signed := Sign(m, "planner-42", "acme")
	var out map[string]any
	bm, _ := json.Marshal(signed)
	json.Unmarshal(bm, &out)
	os.WriteFile(filepath.Join("testdata", "interop-go.json"),
		mustMarshal(out), 0644)
	t.Logf("Go signed %s; Python must verify", m.ID)
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
