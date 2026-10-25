package parley

import (
	"strings"
	"testing"
	"time"
)

// capRoute mirrors the route.plan capability of examples/negociacao.parley:
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
	// request: body deve satisfazer os INPUTS da capability
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	r := BodyContract(m, capRoute())
	if !r["ok"].(bool) {
		t.Fatalf("request com inputs completos deveria passar: %v", r)
	}

	// input obrigatório ausente -> violação
	m2 := *m
	m2.Body = map[string]any{"origin": "Lisboa"}
	r2 := BodyContract(&m2, capRoute())
	if r2["ok"].(bool) {
		t.Fatal("input 'dest' ausente deveria violar o contrato de input")
	}
	if len(r2["violations"].([]string)) != 1 {
		t.Fatalf("esperava 1 violação, obtive %v", r2["violations"])
	}
}

func TestBodyContractProposeOutputs(t *testing.T) {
	// propose: body deve carregar os OUTPUTS declarados
	m := NewMessage("dispatcher-7", "planner-42", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "propose"
	m.Target = "route.plan"
	m.Body = map[string]any{
		"route":     map[string]any{"eta_min": 142},
		"cost_eur":  1.4,
	}
	r := BodyContract(m, capRoute())
	if !r["ok"].(bool) {
		t.Fatalf("propose com output 'route' presente deveria passar: %v", r)
	}
	// 'cost_eur' não declarado -> warning (mundo aberto), não violação
	if len(r["warnings"].([]string)) != 1 {
		t.Fatalf("esperava 1 warning (campo não declarado), obtive %v", r["warnings"])
	}
	// output ausente -> warning, não violação (provider pode omitir opcional)
	m2 := *m
	m2.Body = map[string]any{"cost_eur": 1.4}
	r2 := BodyContract(&m2, capRoute())
	if r2["ok"] != true {
		t.Fatalf("output ausente é reportado, não imposto: %v", r2)
	}
}

func TestBodyContractTypes(t *testing.T) {
	m := NewMessage("a", "b", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "request"
	m.Target = "fare.quote"

	// tipos primitivos corretos
	m.Body = map[string]any{"legs": float64(3), "fast": true}
	if r := BodyContract(m, capPriced()); !r["ok"].(bool) {
		t.Fatalf("tipos corretos deveriam passar: %v", r)
	}

	// tipo errado (string onde INT) -> violação
	m2 := *m
	m2.Body = map[string]any{"legs": "três", "fast": true}
	r2 := BodyContract(&m2, capPriced())
	if r2["ok"].(bool) {
		t.Fatal("legs='três' (STRING onde INT) deveria violar")
	}

	// float não inteiro onde INT -> violação
	m3 := *m
	m3.Body = map[string]any{"legs": 3.5, "fast": true}
	if r3 := BodyContract(&m3, capPriced()); r3["ok"].(bool) {
		t.Fatal("legs=3.5 (float) onde INT deveria violar")
	}

	// bool onde NUMBER -> violação (e NUMBER onde STRING)
	m4 := *m
	m4.Body = map[string]any{"legs": float64(2), "fast": "sim"}
	if r4 := BodyContract(&m4, capPriced()); r4["ok"].(bool) {
		t.Fatal("fast='sim' (STRING onde BOOL) deveria violar")
	}

	// nil -> violação (campo presente mas nulo)
	m5 := *m
	m5.Body = map[string]any{"legs": nil, "fast": true}
	if r5 := BodyContract(&m5, capPriced()); r5["ok"].(bool) {
		t.Fatal("legs=null deveria violar (campo nulo)")
	}
}

func TestBodyContractKindSelection(t *testing.T) {
	// request -> input; propose -> output; inform -> input; accept -> output
	cases := map[string]string{
		"request": "input", "inform": "input",
		"propose": "output", "counter": "output",
		"accept":  "output", "commit": "output",
	}
	for act, want := range cases {
		if got := ContractKind(act); got != want {
			t.Errorf("ContractKind(%q) = %q, esperado %q", act, got, want)
		}
	}
	if ContractKind("ask") != "" || ContractKind("delegate") != "" {
		t.Error("acts sem contrato de corpo deveriam retornar vazio")
	}
	// act sem contrato: nenhum lado é checado
	m := NewMessage("a", "b", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Act = "ask"
	m.Target = "route.plan"
	m.Body = map[string]any{"x": 1}
	r := BodyContract(m, capRoute())
	if !r["ok"].(bool) {
		t.Fatalf("act 'ask' não deve acionar contrato de corpo: %v", r)
	}
}

func TestValidateWithBodyContract(t *testing.T) {
	// Validate com contracts: request com body fora do contrato -> warning
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa"} // 'dest' ausente
	AssignID(m)
	signed := Sign(m, "planner-42", "acme")
	contracts := map[string]*Capability{"route.plan": capRoute()}

	r := ValidateWith(signed, nil, time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC), contracts)
	if !r["schema_ok"].(bool) || !r["signature_ok"].(bool) {
		t.Fatalf("mensagem deveria validar schema/assinatura: %v", r)
	}
	bc, ok := r["body_contract"].(map[string]any)
	if !ok {
		t.Fatal("body_contract ausente no resultado do Validate")
	}
	if bc["ok"].(bool) {
		t.Fatal("body sem 'dest' deveria violar o contrato de input")
	}
	warns := r["warnings"].([]string)
	found := false
	for _, w := range warns {
		if strings.Contains(w, "contrato de corpo") {
			found = true
		}
	}
	if !found {
		t.Fatalf("violação de contrato de corpo não reportada em warnings: %v", warns)
	}

	// sem contracts: nenhum body_contract no resultado (comportamento antigo)
	r2 := Validate(signed, nil, time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC))
	if _, ok := r2["body_contract"]; ok {
		t.Fatal("Validate sem contracts não deve produzir body_contract")
	}
}

func TestValidateWithBodyContractPropose(t *testing.T) {
	// propose com output 'route' presente: contrato de output ok
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
		t.Fatalf("propose com 'route' presente deveria satisfazer o output: %v", bc)
	}
}
