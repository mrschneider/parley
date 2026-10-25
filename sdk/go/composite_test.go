package parley

import (
	"testing"
	"time"
)

func compositeV1Message(t *testing.T) *Message {
	t.Helper()
	m := NewMessage("planner-42", "dispatcher-7", time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC))
	m.V = 1 // v1: composite checks
	m.Tenant = "acme"
	m.Act = "propose"
	m.Target = "route.plan"
	m.Parent = "msg:0000000000000001"
	m.Body = map[string]any{
		"route":   map[string]any{"eta_min": 100, "toll_free": true},
		"lanes":   []any{"A25", "IP3"},
		"weather": "rain",
	}
	m.Expect = []Clause{{
		ID: "composite",
		Check: &Check{
			AllOf: []Check{
				{Field: "route.eta_min", Op: "lte", Value: 150},
				{AnyOf: []Check{
					{Field: "route.toll_free", Op: "eq", Value: true},
					{Field: "route.eta_min", Op: "lte", Value: 80},
				}},
			},
		},
		Weight: 1,
	}}
	AssignID(m)
	return Sign(m, "planner-42", "acme")
}

func TestSchemaV1AcceptsCompositeChecks(t *testing.T) {
	m := compositeV1Message(t)
	if !SchemaValid(MessageToAny(m)) {
		out := Validate(m, nil, time.Now())
		t.Fatalf("valid v1 message rejected: %v", out["schema_error"])
	}
	ok, detail := CheckClause(m.Expect[0], m)
	if !ok {
		t.Fatalf("composite check should pass: %s", detail)
	}
}

func TestCompositeCheckAllOfAndAnyOfSemantics(t *testing.T) {
	m := compositeV1Message(t)
	m.Body["route"] = map[string]any{"eta_min": 151, "toll_free": true}
	ok, _ := CheckClause(m.Expect[0], m)
	if ok {
		t.Fatal("all_of must fail when one arm fails")
	}

	m.Body["route"] = map[string]any{"eta_min": 140, "toll_free": false}
	ok, _ = CheckClause(m.Expect[0], m)
	if ok {
		t.Fatal("any_of must fail when no arm matches")
	}

	m.Body["route"] = map[string]any{"eta_min": 140, "toll_free": true}
	ok, detail := CheckClause(m.Expect[0], m)
	if !ok {
		t.Fatalf("composite check should pass: %s", detail)
	}
}

func TestSchemaV0RejectsCompositeChecks(t *testing.T) {
	m := compositeV1Message(t)
	m.V = 0
	if SchemaValid(MessageToAny(m)) {
		t.Fatal("composite check must not validate under v0")
	}
}
