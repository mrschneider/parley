package parley

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schema/parley-v0.schema.json
var schemaV0JSON []byte

//go:embed schema/parley-v1.schema.json
var schemaV1JSON []byte

var (
	schemaOnce   sync.Once
	schemaV0     *jsonschema.Schema
	schemaV1     *jsonschema.Schema
	schemaLoadEr error
)

// Validate checks a message the same way the Python reference does:
// canonical schema (draft 2020-12), signature, tenant, deadline, and the
// hybrid contract (parent's 'expect' clauses evaluated against this
// message). Returns a flat result map for JSON-friendly reporting.
func Validate(m *Message, parent *Message, now time.Time) map[string]any {
	return ValidateWith(m, parent, now, nil)
}

// ValidateWith is Validate with an optional capability contract: when
// contracts is non-nil and m.Target names one of them, the body is
// validated against the capability's input/output contract (see
// BodyContract) — the "validated against the capability's input/output
// contract" promise of the schema.
func ValidateWith(m *Message, parent *Message, now time.Time, contracts map[string]*Capability) map[string]any {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	out := map[string]any{"id": m.ID, "act": m.Act, "warnings": []string{}}

	if !checkSchema(m, out) {
		return out
	}

	ok, why := Verify(m)
	out["signature_ok"] = ok
	out["sig_msg"] = why

	out["tenant_ok"] = m.Tenant != ""
	if m.Tenant == "" {
		out["warnings"] = append(out["warnings"].([]string), "sem tenant (exigível em deploy multi-tenant)")
	}

	if m.Deadline != "" {
		if dl, err := time.Parse("2006-01-02T15:04:05Z", m.Deadline); err == nil && dl.Before(now) {
			out["warnings"] = append(out["warnings"].([]string), "deadline expirado: "+m.Deadline)
		}
	}

	// capability body contract (when the target names a known capability)
	if contracts != nil && m.Target != "" {
		if cap, ok := contracts[m.Target]; ok {
			out["body_contract"] = BodyContract(m, cap)
			bc := out["body_contract"].(map[string]any)
			for _, w := range bc["violations"].([]string) {
				out["warnings"] = append(out["warnings"].([]string), "contrato de corpo: "+w)
			}
			for _, w := range bc["warnings"].([]string) {
				out["warnings"] = append(out["warnings"].([]string), "contrato de corpo: "+w)
			}
		}
	}

	// contract: parent's 'expect' clauses evaluated against THIS message
	if parent != nil && len(parent.Expect) > 0 && isContractAct(m.Act) {
		accepted := map[string]bool{}
		for _, c := range m.Clauses {
			accepted[c] = true
		}
		clauses := []map[string]any{}
		for _, c := range parent.Expect {
			switch {
			case c.NL != "":
				binding := accepted[c.ID]
				detail := "NÃO aceita explicitamente -> não vinculante"
				if binding {
					detail = "aceite explícito"
				}
				clauses = append(clauses, map[string]any{
					"id": c.ID, "nl": c.NL, "binding": binding, "detail": detail,
				})
			default:
				okc, detail := CheckClause(c, m)
				clauses = append(clauses, map[string]any{
					"id": c.ID, "check": c.Check, "ok": okc, "detail": detail,
				})
			}
		}
		out["clauses"] = clauses
	}
	return out
}

// isContractAct reports whether the act engages the hybrid contract (the
// sender-side acts that are evaluated against the parent's 'expect'
// clauses). 'reject'/'cancel' settle the negotiation without delivering a
// body; 'inform'/'ask'/'delegate'/'verify' carry no offer.
func isContractAct(act string) bool {
	switch act {
	case "propose", "counter", "accept", "commit":
		return true
	}
	return false
}

// CheckClause evaluates one 'check' clause against a message. It supports
// v0 leaf checks and v1 all_of/any_of compositions (which may be combined).
func CheckClause(cl Clause, m *Message) (bool, string) {
	if cl.Check == nil {
		return true, "nl (sem verificação de máquina)"
	}
	return checkAny(*cl.Check, m)
}

func checkAny(chk Check, m *Message) (bool, string) {
	var allResults, anyResults []string
	allOK, anyOK := true, false
	anyPresent := false
	for _, child := range chk.AllOf {
		ok, detail := checkAny(child, m)
		allResults = append(allResults, detail)
		if !ok {
			allOK = false
		}
	}
	if len(chk.AllOf) > 0 {
		anyPresent = true
	}
	for _, child := range chk.AnyOf {
		ok, detail := checkAny(child, m)
		anyResults = append(anyResults, detail)
		if ok {
			anyOK = true
		}
	}
	if len(chk.AnyOf) > 0 {
		anyPresent = true
	}
	if anyPresent {
		// Composite form: both groups (when present) must hold.
		compositeOK := allOK && (len(chk.AnyOf) == 0 || anyOK)
		detail := fmt.Sprintf("all_of=%v any_of=%v", allResults, anyResults)
		return compositeOK, detail
	}
	return checkLeaf(chk, m)
}

func checkLeaf(chk Check, m *Message) (bool, string) {
	var actual any
	if chk.Field == "confidence" {
		if m.Confidence != nil {
			actual = *m.Confidence
		}
	} else if v, ok := GetPath(m.Body, chk.Field); ok {
		actual = v
	}

	switch chk.Op {
	case "not_null":
		return actual != nil, fmt.Sprintf("%s=%s (not_null)", chk.Field, pyRepr(actual))
	case "in":
		ok := false
		if list, ok := chk.Value.([]any); ok {
			for _, item := range list {
				if fmt.Sprintf("%v", item) == fmt.Sprintf("%v", actual) {
					ok = true
					break
				}
			}
		}
		return ok, fmt.Sprintf("%s=%s (in %s)", chk.Field, pyRepr(actual), pyRepr(chk.Value))
	case "regex":
		if actual == nil {
			return false, fmt.Sprintf("%s=%s (regex %s)", chk.Field, pyRepr(actual), pyRepr(chk.Value))
		}
		pat, _ := chk.Value.(string)
		ok, _ := regexp.MatchString(pat, fmt.Sprintf("%v", actual))
		return ok, fmt.Sprintf("%s=%s (regex %s)", chk.Field, pyRepr(actual), pyRepr(chk.Value))
	case "lt", "lte", "gt", "gte", "eq":
		if actual == nil {
			return false, "campo ausente: " + chk.Field
		}
		a, aok := asFloat(actual)
		v, vok := asFloat(chk.Value)
		if !aok || !vok {
			if chk.Op == "eq" {
				eq := fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", chk.Value)
				return eq, fmt.Sprintf("%s=%s (eq %s)", chk.Field, pyRepr(actual), pyRepr(chk.Value))
			}
			return false, fmt.Sprintf("%s=%s: operador %s exige numéricos", chk.Field, pyRepr(actual), chk.Op)
		}
		var ok bool
		switch chk.Op {
		case "lt":
			ok = a < v
		case "lte":
			ok = a <= v
		case "gt":
			ok = a > v
		case "gte":
			ok = a >= v
		case "eq":
			ok = a == v
		}
		return ok, fmt.Sprintf("%s=%s (%s %s)", chk.Field, pyRepr(actual), chk.Op, pyRepr(chk.Value))
	default:
		return false, "operador desconhecido: " + chk.Op
	}
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// checkSchema validates m against the canonical v0 schema.
func checkSchema(m *Message, out map[string]any) bool {
	b, err := marshalCanonical(MessageToAny(m))
	if err != nil {
		out["schema_ok"] = false
		out["schema_error"] = "marshal: " + err.Error()
		return false
	}
	var parsed any
	if err := json.Unmarshal(b, &parsed); err != nil {
		out["schema_ok"] = false
		out["schema_error"] = "unmarshal: " + err.Error()
		return false
	}
	if err := schemaValidateErr(parsed, m.V); err != nil {
		out["schema_ok"] = false
		out["schema_error"] = err.Error()
		return false
	}
	out["schema_ok"] = true
	return true
}

func schemaValidateErr(v any, version int) error {
	loadSchemas()
	if version <= 0 {
		if err := schemaV0.Validate(v); err != nil {
			return err
		}
		return nil
	}
	if version > 1 {
		return fmt.Errorf("versão de protocolo não suportada: %d", version)
	}
	if err := schemaV1.Validate(v); err != nil {
		return err
	}
	return nil
}

// schemaValid reports whether v validates against the canonical schema for
// its v field (defaults to v0 for non-map/raw test values).
func schemaValid(v any) bool {
	version := 0
	if obj, ok := v.(map[string]any); ok {
		switch n := obj["v"].(type) {
		case float64:
			version = int(n)
		case int64:
			version = int(n)
		case int:
			version = n
		}
	}
	return schemaValidateErr(v, version) == nil
}

// SchemaValid reports whether a raw JSON value validates against the
// canonical schema selected by its v field (exported for tests /
// cross-checks).
func SchemaValid(v any) bool {
	return schemaValid(v)
}

// SchemaValidateError exposes the canonical schema error for diagnostics.
// Version <=0 selects v0; only 0 and 1 are currently supported.
func SchemaValidateError(v any, version int) error {
	return schemaValidateErr(v, version)
}

func loadSchemas() {
	schemaOnce.Do(func() {
		compile := func(raw []byte, id string) *jsonschema.Schema {
			var sch map[string]any
			if err := json.Unmarshal(raw, &sch); err != nil {
				panic("parley: schema: " + err.Error())
			}
			compiler := jsonschema.NewCompiler()
			// Python's jsonschema does NOT enforce 'format' by default, and
			// this library only asserts formats when AssertFormat() is called
			// (it is not) — so date-time 'format' is descriptive only.
			if err := compiler.AddResource(id, sch); err != nil {
				panic("parley: schema resource: " + err.Error())
			}
			s, err := compiler.Compile(id)
			if err != nil {
				panic("parley: schema: " + err.Error())
			}
			return s
		}
		schemaV0 = compile(schemaV0JSON, "https://example.org/parley/v0/message.schema.json")
		schemaV1 = compile(schemaV1JSON, "https://example.org/parley/v1/message.schema.json")
	})
}

// fmtFloat formats a float the way Python's repr does (1.0 -> "1", 0.93 -> "0.93").
func fmtFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e16 && f > -1e16 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
