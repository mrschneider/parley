package transport

import (
	"encoding/json"
	"testing"
	"time"

	parley "example.org/parley-go"
)

func benchSigned(b *testing.B, act, target string, parent *parley.Message) []byte {
	b.Helper()
	m := parley.NewMessage("agent-1", "agent-2", time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = act
	m.Target = target
	if parent != nil {
		m.Parent = parent.ID
	}
	conf := 0.9
	m.Confidence = &conf
	m.Body = map[string]any{"origin": "Lisbon", "dest": "Porto", "eta": int64(140)}
	m.Expect = []parley.Clause{
		{ID: "c1", Check: &parley.Check{Field: "confidence", Op: "gt", Value: 0.85}, Weight: 1.0},
		{ID: "c3", NL: "Avoid roadworks in the next 24h", Weight: 0.9},
	}
	parley.AssignID(m)
	signed := parley.Sign(m, "agent-1", "acme")
	raw, err := json.Marshal(signed)
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

// BenchmarkInboxDeliverSteadyState: steady-state delivery (no retention),
// with the store growing — measures the full hot path per delivery.
func BenchmarkInboxDeliverSteadyState(b *testing.B) {
	b.ReportAllocs()
	in := NewInbox()
	parent := benchSigned(b, "request", "route.plan", nil)
	if res := in.Deliver(parent); res.Status != StatusAccepted {
		b.Fatal(res.Reason)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw := benchSigned(b, "propose", "route.plan", &parley.Message{ID: "parent"})
		if res := in.Deliver(raw); res.Status != StatusAccepted && res.Status != StatusDuplicate {
			b.Fatal(res.Reason)
		}
	}
}

// BenchmarkInboxDeliverWithRetention exercises the sweep path on every
// delivery (retention policy active).
func BenchmarkInboxDeliverWithRetention(b *testing.B) {
	b.ReportAllocs()
	in := NewInbox(WithRetention(0, 1000))
	parent := benchSigned(b, "request", "route.plan", nil)
	if res := in.Deliver(parent); res.Status != StatusAccepted {
		b.Fatal(res.Reason)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw := benchSigned(b, "propose", "route.plan", &parley.Message{ID: "parent"})
		if res := in.Deliver(raw); res.Status != StatusAccepted && res.Status != StatusDuplicate {
			b.Fatal(res.Reason)
		}
	}
}
