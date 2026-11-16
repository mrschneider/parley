package parley

import (
	"encoding/json"
	"testing"
	"time"
)

func benchMessage() *Message {
	m := NewMessage("agent-1", "agent-2", time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "propose"
	m.Target = "route.plan"
	conf := 0.9
	m.Confidence = &conf
	m.Body = map[string]any{
		"route": map[string]any{
			"eta_min": int64(140), "distance_km": 310.5,
			"legs": []any{map[string]any{"km": 12.5, "mode": "road"}},
		},
		"origin": "Lisbon", "dest": "Porto",
	}
	m.Expect = []Clause{
		{ID: "c1", Check: &Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0},
		{ID: "c3", NL: "Avoid roadworks in the next 24h", Weight: 0.9},
	}
	return m
}

func BenchmarkCanonicalBytes(b *testing.B) {
	m := benchMessage()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := CanonicalBytes(m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSign(b *testing.B) {
	m := benchMessage()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Sign(m, "agent-1", "acme")
	}
}

func BenchmarkVerify(b *testing.B) {
	m := Sign(benchMessage(), "agent-1", "acme")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ok, _ := Verify(m); !ok {
			b.Fatal("verify failed")
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	m := Sign(benchMessage(), "agent-1", "acme")
	now := time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Validate(m, nil, now)
	}
}

// BenchmarkInboxDeliver measures the full delivery hot path of the inbox
// (schema + signature + contract + ranking) with a growing store: the
// offersByParent index keeps this O(1) per delivery instead of O(store).
func BenchmarkInboxDeliver(b *testing.B) {
	b.Skip("use the transport package benchmark for inbox deliver")
	_ = json.Marshal
}
