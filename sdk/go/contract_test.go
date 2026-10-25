package parley

import (
	"strings"
	"testing"
	"time"
)

// capRoute mirrors the route.plan capability of examples/negotiation.parley:
// inputs origin/dest (Location, domain type), output route (Route).
func capRoute() *Capability {
	return &Capability{
		Name:    "route.plan",
		Version: 1,
		Inputs:  []Field{{Name: "origin", Type: "LOCATION"}, {Name: "dest", Type: "LOCATION"}},
		Outputs: []Field{{Name: "route", Type: "ROUTE"}},
		By:      "planner-42",
	}
}

func capPriced() *Capability {
	return &Capability{
		Name:    "fare.quote",
		Version: 1,
		Inputs:  []Field{{Name: "legs", Type: "INT"}, {Name: "fast", Type: "BOOL"}},
		Outputs: []Field{{Name: "price", Type: "NUMBER"}, {Name: "label", Type: "STRING"}},
		By:      "fare-9",
	}
}

func TestBodyContractRequestInputs(t *testing.T) {
	// request: body must satisfy the capability INPUTS
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	r := BodyContract(m, capRoute())
	if !r["ok"].(bool) {
		t.Fatalf("request with complete inputs should pass: %v", r)
	}

	// required input missing -> violation
	m2 := *m
	m2.Body = map[string]any{"origin": "Lisboa"}
	r2 := BodyContract(&m2, capRoute())
	if r2["ok"].(bool) {
		t.Fatal("input 'dest' missing should violate the input contract")
	}
	if len(r2["violations"].([]string)) != 1 {
		t.Fatalf("expected 1 violation, got %v", r2["violations"])
	}
}

func TestBodyContractProposeOutputs(t *testing.T) {
	// propose: body must carry the declared OUTPUTS
	m := NewMessage("dispatcher-7", "planner-42", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "propose"
	m.Target = "route.plan"
	m.Body = map[string]any{
		"route":    map[string]any{"eta_min": 142},
		"cost_eur": 1.4,
	}
	r := BodyContract(m, capRoute())
	if !r["ok"].(bool) {
		t.Fatalf("propose with output 'route' present should pass: %v", r)
	}
	// 'cost_eur' not declared -> warning (open world), not a violation
	if len(r["warnings"].([]string)) != 1 {
		t.Fatalf("expected 1 warning (undeclared field), got %v", r["warnings"])
	}
	// output missing -> warning, not a violation (provider may omit an optional)
	m2 := *m
	m2.Body = map[string]any{"cost_eur": 1.4}
	r2 := BodyContract(&m2, capRoute())
	if r2["ok"] != true {
		t.Fatalf("missing output is reported, not enforced: %v", r2)
	}
}

func TestBodyContractTypes(t *testing.T) {
	m := NewMessage("a", "b", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "request"
	m.Target = "fare.quote"

	// correct primitive types
	m.Body = map[string]any{"legs": float64(3), "fast": true}
	if r := BodyContract(m, capPriced()); !r["ok"].(bool) {
		t.Fatalf("correct types should pass: %v", r)
	}

	// wrong type (string where INT) -> violation
	m2 := *m
	m2.Body = map[string]any{"legs": "three", "fast": true}
	r2 := BodyContract(&m2, capPriced())
	if r2["ok"].(bool) {
		t.Fatal("legs='three' (STRING where INT) should violate")
	}

	// non-integer float where INT -> violation
	m3 := *m
	m3.Body = map[string]any{"legs": 3.5, "fast": true}
	if r3 := BodyContract(&m3, capPriced()); r3["ok"].(bool) {
		t.Fatal("legs=3.5 (float) where INT should violate")
	}

	// bool where NUMBER -> violation (and NUMBER where STRING)
	m4 := *m
	m4.Body = map[string]any{"legs": float64(2), "fast": "yes"}
	if r4 := BodyContract(&m4, capPriced()); r4["ok"].(bool) {
		t.Fatal("fast='yes' (STRING where BOOL) should violate")
	}

	// nil -> violation (field present but null)
	m5 := *m
	m5.Body = map[string]any{"legs": nil, "fast": true}
	if r5 := BodyContract(&m5, capPriced()); r5["ok"].(bool) {
		t.Fatal("legs=null should violate (null field)")
	}
}

func TestBodyContractKindSelection(t *testing.T) {
	// request -> input; propose -> output; inform -> input; accept -> output
	cases := map[string]string{
		"request": "input", "inform": "input",
		"propose": "output", "counter": "output",
		"accept": "output", "commit": "output",
	}
	for act, want := range cases {
		if got := ContractKind(act); got != want {
			t.Errorf("ContractKind(%q) = %q, expected %q", act, got, want)
		}
	}
	if ContractKind("ask") != "" || ContractKind("delegate") != "" {
		t.Error("acts without a body contract should return empty")
	}
	// act without a contract: neither side is checked
	m := NewMessage("a", "b", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "ask"
	m.Target = "route.plan"
	m.Body = map[string]any{"x": 1}
	r := BodyContract(m, capRoute())
	if !r["ok"].(bool) {
		t.Fatalf("act 'ask' must not trigger a body contract: %v", r)
	}
}

func TestValidateWithBodyContract(t *testing.T) {
	// Validate with contracts: request with body outside the contract -> warning
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa"} // 'dest' missing
	AssignID(m)
	signed := Sign(m, "planner-42", "acme")
	contracts := map[string]*Capability{"route.plan": capRoute()}

	r := ValidateWith(signed, nil, time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC), contracts)
	if !r["schema_ok"].(bool) || !r["signature_ok"].(bool) {
		t.Fatalf("message should validate schema/signature: %v", r)
	}
	bc, ok := r["body_contract"].(map[string]any)
	if !ok {
		t.Fatal("body_contract missing from the Validate result")
	}
	if bc["ok"].(bool) {
		t.Fatal("body without 'dest' should violate the input contract")
	}
	warns := r["warnings"].([]string)
	found := false
	for _, w := range warns {
		if strings.Contains(w, "body contract") {
			found = true
		}
	}
	if !found {
		t.Fatalf("body contract violation not reported in warnings: %v", warns)
	}

	// without contracts: no body_contract in the result (legacy behavior)
	r2 := Validate(signed, nil, time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	if _, ok := r2["body_contract"]; ok {
		t.Fatal("Validate without contracts must not produce body_contract")
	}
}

func TestValidateWithBodyContractPropose(t *testing.T) {
	// propose with output 'route' present: output contract ok
	m := NewMessage("dispatcher-7", "planner-42", time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "propose"
	m.Target = "route.plan"
	m.Body = map[string]any{"route": map[string]any{"eta_min": 142}}
	AssignID(m)
	signed := Sign(m, "dispatcher-7", "acme")
	contracts := map[string]*Capability{"route.plan": capRoute()}
	r := ValidateWith(signed, nil, time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC), contracts)
	bc := r["body_contract"].(map[string]any)
	if !bc["ok"].(bool) {
		t.Fatalf("propose with 'route' present should satisfy the output: %v", bc)
	}
}
