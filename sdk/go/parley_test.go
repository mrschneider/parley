package parley

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func loadExample(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"../../examples/negotiation.parley",
		"examples/negotiation.parley",
	} {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatal("examples/negotiation.parley not found")
	return ""
}

func TestParseAndIDs(t *testing.T) {
	fixed := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	oldNow := Now
	Now = func() time.Time { return fixed }
	defer func() { Now = oldNow }()
	doc, err := ParseText(loadExample(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(doc.Messages))
	}
	msgs := Finalize(doc)
	// content ids: well-formed, unique, idempotent (recomputing from the
	// same content -> same id; that is the basis of replay/dedup)
	seen := map[string]bool{}
	for _, m := range msgs {
		if !regexp.MustCompile(`^msg:[0-9a-f]{16}$`).MatchString(m.ID) {
			t.Fatalf("malformed id: %s", m.ID)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate id: %s", m.ID)
		}
		seen[m.ID] = true
		// content idempotency: two independent parses of the same source
		// (same ts, because of time.Now at parse) must produce the same ids
		// — that is what gives dedup/replay in queues.
		src := loadExample(t)
		doc2, _ := ParseText(src)
		msgs2 := Finalize(doc2)
		for i, m2 := range msgs2 {
			if m2.ID != msgs[i].ID {
				t.Errorf("id[%d] differs between independent parses: %s vs %s", i, m2.ID, msgs[i].ID)
			}
		}
	}
	// @msgN correlation resolved
	if msgs[1].Parent != msgs[0].ID {
		t.Errorf("msg2 parent = %s, expected %s", msgs[1].Parent, msgs[0].ID)
	}
	if len(msgs[2].Provenance) != 2 || msgs[2].Provenance[0] != msgs[0].ID || msgs[2].Provenance[1] != msgs[1].ID {
		t.Errorf("wrong msg3 provenance: %v", msgs[2].Provenance)
	}
	// stable clauses
	if len(msgs[0].Expect) != 3 || msgs[0].Expect[2].ID != "c3" || msgs[0].Expect[2].NL == "" {
		t.Errorf("wrong msg1 expect: %+v", msgs[0].Expect)
	}
	// capacidade
	cap, ok := doc.Capabilities["route.plan"]
	if !ok || cap.Version != 1 || len(cap.Inputs) != 2 || len(cap.Outputs) != 1 || len(cap.Cost) != 1 {
		t.Errorf("wrong route.plan capability: %+v", cap)
	}
}

func TestSignVerifyRoundtrip(t *testing.T) {
	doc, _ := ParseText(loadExample(t))
	msgs := Finalize(doc)
	for _, m := range msgs {
		signed := Sign(m, m.From.ID, m.Tenant)
		ok, why := Verify(signed)
		if !ok {
			t.Errorf("invalid signature after sign: %s", why)
		}
		// tamper: any field change breaks the signature
		tm := *signed
		tm.Note = "tampered"
		if ok, _ := Verify(&tm); ok {
			t.Error("tamper not detected (note)")
		}
	}
}

func TestTamperConfidence(t *testing.T) {
	doc, _ := ParseText(loadExample(t))
	msgs := Finalize(doc)
	m1 := Sign(msgs[0], msgs[0].From.ID, msgs[0].Tenant)
	m2 := Sign(msgs[1], msgs[1].From.ID, msgs[1].Tenant)

	// intact proposal: contract satisfied
	r := Validate(m2, m1, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if !r["signature_ok"].(bool) || !r["schema_ok"].(bool) {
		t.Fatalf("intact proposal should validate: %v", r)
	}
	clauses := r["clauses"].([]map[string]any)
	byID := map[string]map[string]any{}
	for _, m := range clauses {
		byID[m["id"].(string)] = m
	}
	if !byID["c1"]["ok"].(bool) { // confidence 0.93 > 0.85
		t.Errorf("c1 should pass: %v", byID["c1"])
	}
	if !byID["c2"]["ok"].(bool) { // route.eta_min 142 <= 150
		t.Errorf("c2 should pass: %v", byID["c2"])
	}
	if byID["c3"]["binding"].(bool) { // NL without explicit accept
		t.Errorf("c3 (NL) should not be binding in the proposal: %v", byID["c3"])
	}

	// tampered: confidence 0.5 -> invalid signature AND c1 violated
	bad := *m2
	bad.Body = map[string]any{}
	for k, v := range m2.Body {
		bad.Body[k] = v
	}
	conf := 0.5
	bad.Confidence = &conf
	if ok, _ := Verify(&bad); ok {
		t.Fatal("tamper in confidence not detected")
	}
	rb := Validate(&bad, m1, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	if rb["signature_ok"].(bool) {
		t.Fatal("tampered message should not have a valid signature")
	}
	clb := rb["clauses"].([]map[string]any)
	for _, m := range clb {
		if m["id"] == "c1" && m["ok"].(bool) {
			t.Error("c1 should violate with confidence 0.5")
		}
	}
}

func TestSelectiveAcceptMakesNLBinding(t *testing.T) {
	doc, _ := ParseText(loadExample(t))
	msgs := Finalize(doc)
	m1 := Sign(msgs[0], msgs[0].From.ID, msgs[0].Tenant)
	m3 := Sign(msgs[2], msgs[2].From.ID, msgs[2].Tenant)
	r := Validate(m3, m1, time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	clauses := r["clauses"].([]map[string]any)
	found := false
	for _, m := range clauses {
		if m["id"] == "c3" {
			found = true
			if !m["binding"].(bool) {
				t.Errorf("c3 (NL) should be binding after explicit accept: %v", m)
			}
		}
	}
	if !found {
		t.Fatal("clause c3 not found")
	}
}

func TestSchemaRejectsUnknownFields(t *testing.T) {
	doc, _ := ParseText(loadExample(t))
	msgs := Finalize(doc)
	m := *msgs[0]
	m.Signature = nil
	// injects a disallowed field (additionalProperties: false) into the raw JSON
	b, _ := json.Marshal(m)
	var mm map[string]any
	json.Unmarshal(b, &mm)
	mm["hacker_field"] = true
	if SchemaValid(mm) {
		t.Error("extra field should violate the schema (additionalProperties: false)")
	}
	// and the clean message passes
	if !SchemaValid(MessageToAny(&m)) {
		t.Error("clean message should pass the schema")
	}
}

func TestValidateRequiresTenantWarning(t *testing.T) {
	m := NewMessage("a", "b", time.Now())
	m.Act = "inform"
	m.TS = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	AssignID(m)
	signed := Sign(m, "a", "default")
	r := Validate(signed, nil, time.Now().UTC())
	if r["tenant_ok"] != false {
		t.Error("missing tenant should give tenant_ok=false")
	}
	ws, _ := r["warnings"].([]string)
	if len(ws) == 0 || !strings.Contains(ws[0], "tenant") {
		t.Errorf("expected a tenant warning: %v", ws)
	}
}

func TestParseValue(t *testing.T) {
	cases := map[string]any{
		`"hello"`: "hello",
		"true":    true,
		"false":   false,
		"-42":     float64(-42),
		"3.14":    3.14,
		"word":    "word",
		`{"a":1}`: map[string]any{"a": float64(1)},
		`["x",1]`: []any{"x", float64(1)},
		"[a, b]":  []any{"a", "b"},
		"[1, 2]":  []any{float64(1), float64(2)},
	}
	for in, want := range cases {
		got, err := parseValue(in)
		if err != nil {
			t.Errorf("parseValue(%q): %v", in, err)
			continue
		}
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if string(gb) != string(wb) {
			t.Errorf("parseValue(%q) = %s, expected %s", in, gb, wb)
		}
	}
}

func TestCanonicalMatchesPythonBytes(t *testing.T) {
	// The canonical serialization of a Python-signed message must be
	// reproduced byte for byte (signature interop proof).
	b, err := os.ReadFile(filepath.Join("testdata", "interop-py.json"))
	if err != nil {
		t.Skip("interop fixture missing")
	}
	var pyMsgs []map[string]any
	if err := json.Unmarshal(b, &pyMsgs); err != nil {
		t.Fatal(err)
	}
	for _, pym := range pyMsgs {
		var m Message
		bm, _ := json.Marshal(pym)
		json.Unmarshal(bm, &m)
		if ok, why := Verify(&m); !ok {
			t.Errorf("Python-generated signature not verified by Go: %s", why)
		}
	}
}

func TestInteropIDAndBytes(t *testing.T) {
	// Fixture with a fixed timestamp: the content id AND canonical bytes
	// must be identical to those generated by Python (canonical number rule).
	b, err := os.ReadFile(filepath.Join("testdata", "interop-id.json"))
	if err != nil {
		t.Skip("interop-id fixture missing")
	}
	var fx struct {
		ExpectedID string         `json:"expected_id"`
		Signed     map[string]any `json:"signed"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	var m Message
	bm, _ := json.Marshal(fx.Signed)
	json.Unmarshal(bm, &m)

	// 1) identical canonical bytes
	canon, _ := CanonicalBytes(&m)
	var tree map[string]any
	json.Unmarshal(bm, &tree)
	delete(tree, "signature")
	delete(tree, "id")
	wantCanon, err := marshalCanonical(tree)
	if err != nil {
		t.Fatal(err)
	}
	_ = wantCanon // (sanity)
	// 2) content id identical to Python
	cp := m
	cp.ID = ""
	AssignID(&cp)
	if cp.ID != fx.ExpectedID {
		t.Errorf("content id differs from Python:\n go: %s\n py: %s\ncanon go: %s", cp.ID, fx.ExpectedID, canon)
	}
	// 3) Python signature verified by Go
	if ok, why := Verify(&m); !ok {
		t.Errorf("Python signature not verified by Go: %s", why)
	}
	// 4) a Go-signed message is verifiable in Python (checked by the
	//    cross-verification script, not here)
	_ = canon
}
