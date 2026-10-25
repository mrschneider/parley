// Package parley implements PARLEY v0 — a language/protocol for
// communication between AI agents.
//
// PARLEY is a domain-specific language, platform-agnostic (HTTP, WebSocket,
// queues). The canonical JSON IS the protocol: any platform that can read
// the schema speaks PARLEY.
//
// Trust model (multi-tenant, UNTRUSTED agents):
//   - message id is content-addressed (sha256) -> idempotency & replay detection
//   - signature over the canonical serialization (every field except "signature")
//   - "tenant" in the envelope, enforced by validators
//   - provenance: chain of message ids this one is based on
//
// Hybrid contracts:
//   - clause "check": machine-validated (dotted path in body, operator, value)
//   - clause "nl":    natural language; becomes BINDING only when the
//     counterparty lists its id explicitly in ACCEPT (clauses)
package parley

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version is the PARLEY protocol version.
const Version = 0

// Acts are the speech acts (KQML lineage, LLM-era).
var Acts = []string{
	"inform", "ask", "request", "propose", "accept", "reject", "counter",
	"commit", "delegate", "verify", "cancel",
}

// AgentRef identifies an agent (optionally within a tenant).
type AgentRef struct {
	ID     string `json:"id"`
	Tenant string `json:"tenant,omitempty"`
}

// Field is a typed input/output of a capability.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Check is a machine-checkable constraint over body fields.
type Check struct {
	Field string      `json:"field"`
	Op    string      `json:"op"`
	Value interface{} `json:"value,omitempty"`
}

// Clause is a hybrid contract clause: machine-checkable (Check) and/or
// natural-language (NL). An NL clause binds only after the counterparty's
// explicit accept (prevents silent ambiguity becoming a contract).
type Clause struct {
	ID     string  `json:"id,omitempty"`
	Check  *Check  `json:"check,omitempty"`
	NL     string  `json:"nl,omitempty"`
	Weight float64 `json:"weight"`
}

// Fallback is routed to if the primary declines or misses the deadline.
type Fallback struct {
	To     AgentRef `json:"to"`
	Reason string   `json:"reason,omitempty"`
}

// Signature carries a signature over the canonical serialization.
type Signature struct {
	Alg string `json:"alg"` // "ed25519" | "hmac-sha256"
	Key string `json:"key"` // public key id of the sender
	Sig string `json:"sig"` // base64
}

// Message is one PARLEY v0 canonical message.
type Message struct {
	V          int            `json:"v"`
	ID         string         `json:"id"`
	Parent     string         `json:"parent,omitempty"`
	From       AgentRef       `json:"from"`
	To         AgentRef       `json:"to"`
	Tenant     string         `json:"tenant,omitempty"`
	TS         string         `json:"ts"`
	Act        string         `json:"act"`
	Target     string         `json:"target,omitempty"`
	Intent     string         `json:"intent,omitempty"`
	Body       map[string]any `json:"body,omitempty"`
	Note       string         `json:"note,omitempty"`
	Confidence *float64       `json:"confidence,omitempty"`
	Deadline   string         `json:"deadline,omitempty"`
	Expect     []Clause       `json:"expect,omitempty"`
	Fallback   *Fallback      `json:"fallback,omitempty"`
	Clauses    []string       `json:"clauses,omitempty"`
	Provenance []string       `json:"provenance,omitempty"`
	Signature  *Signature     `json:"signature,omitempty"`
}

// NewMessage starts a message between two agent ids.
func NewMessage(fromID, toID string, ts time.Time) *Message {
	return &Message{
		V:    Version,
		From: AgentRef{ID: fromID},
		To:   AgentRef{ID: toID},
		TS:   ts.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z"),
	}
}

// ---------------------------------------------------------------- serialization
//
// CANONICAL NUMBER RULE (protocol): a value is serialized by value, not by
// token — integral values as plain integers ("150", never "150.0" or "1.0"),
// non-integral values with the shortest round-trip representation ("0.85").
// Both the Python and Go implementations MUST follow this rule, which makes
// the canonical bytes (and therefore message ids and signatures)
// platform-independent.

func marshalCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case int:
		buf.WriteString(strconv.Itoa(x))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case float64:
		buf.WriteString(canonicalFloat(x))
	case json.Number:
		if f, err := x.Float64(); err == nil && (strings.ContainsAny(x.String(), ".eE")) {
			buf.WriteString(canonicalFloat(f))
		} else if n, err := x.Int64(); err == nil {
			buf.WriteString(strconv.FormatInt(n, 10))
		} else {
			return fmt.Errorf("número canônico inválido: %s", x)
		}
	case string:
		encodeJSONString(buf, x)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeJSONString(buf, k)
			buf.WriteByte(':')
			if err := encodeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("tipo não serializável canonicamente: %T", v)
	}
	return nil
}

// canonicalFloat implements the CANONICAL NUMBER RULE for floats.
func canonicalFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// encodeJSONString escapes a string exactly like Python's
// json.dumps(..., ensure_ascii=False).
func encodeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, "\\u%04x", r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// MessageToAny builds the plain-value tree of m preserving int/float
// distinctness (needed for the canonical number rule).
func MessageToAny(m *Message) map[string]any {
	out := map[string]any{
		"v":    int64(m.V),
		"from": agentRefToAny(m.From),
		"to":   agentRefToAny(m.To),
		"ts":   m.TS,
	}
	if m.ID != "" {
		out["id"] = m.ID
	}
	if m.Parent != "" {
		out["parent"] = m.Parent
	}
	if m.Tenant != "" {
		out["tenant"] = m.Tenant
	}
	if m.Act != "" {
		out["act"] = m.Act
	}
	if m.Target != "" {
		out["target"] = m.Target
	}
	if m.Intent != "" {
		out["intent"] = m.Intent
	}
	if m.Body != nil {
		out["body"] = m.Body
	}
	if m.Note != "" {
		out["note"] = m.Note
	}
	if m.Confidence != nil {
		out["confidence"] = *m.Confidence
	}
	if m.Deadline != "" {
		out["deadline"] = m.Deadline
	}
	if len(m.Expect) > 0 {
		arr := make([]any, 0, len(m.Expect))
		for _, c := range m.Expect {
			arr = append(arr, clauseToAny(c))
		}
		out["expect"] = arr
	}
	if m.Fallback != nil {
		fb := map[string]any{"to": agentRefToAny(m.Fallback.To)}
		if m.Fallback.Reason != "" {
			fb["reason"] = m.Fallback.Reason
		}
		out["fallback"] = fb
	}
	if len(m.Clauses) > 0 {
		out["clauses"] = stringsToAny(m.Clauses)
	}
	if len(m.Provenance) > 0 {
		out["provenance"] = stringsToAny(m.Provenance)
	}
	if m.Signature != nil {
		out["signature"] = map[string]any{
			"alg": m.Signature.Alg, "key": m.Signature.Key, "sig": m.Signature.Sig,
		}
	}
	return out
}

func stringsToAny(ss []string) []any {
	out := make([]any, 0, len(ss))
	for _, s := range ss {
		out = append(out, s)
	}
	return out
}

func agentRefToAny(a AgentRef) map[string]any {
	out := map[string]any{"id": a.ID}
	if a.Tenant != "" {
		out["tenant"] = a.Tenant
	}
	return out
}

func clauseToAny(c Clause) map[string]any {
	out := map[string]any{}
	if c.ID != "" {
		out["id"] = c.ID
	}
	if c.Check != nil {
		ch := map[string]any{"field": c.Check.Field, "op": c.Check.Op}
		if c.Check.Value != nil {
			ch["value"] = c.Check.Value
		}
		out["check"] = ch
	}
	if c.NL != "" {
		out["nl"] = c.NL
	}
	out["weight"] = c.Weight
	return out
}

// CanonicalBytes returns the canonical serialization of m: every field
// except "signature", keys sorted, compact, canonical number rule. This
// byte string is what gets signed and what the message id is derived from.
func CanonicalBytes(m *Message) ([]byte, error) {
	tree := MessageToAny(m)
	delete(tree, "signature")
	if m.ID == "" {
		delete(tree, "id")
	}
	return marshalCanonical(tree)
}

// ---------------------------------------------------------------- signing

// DeriveKey derives a deterministic TEST key from agent identity.
// Production: use a real KMS / key wallet — NEVER derive from id.
func DeriveKey(agentID, tenant string) (alg string, priv ed25519.PrivateKey, seed []byte) {
	if tenant == "" {
		tenant = "default"
	}
	sum := sha256.Sum256([]byte(tenant + ":" + agentID))
	return "ed25519", ed25519.NewKeyFromSeed(sum[:]), sum[:]
}

// Sign signs m with the deterministic test key of agentID/tenant and
// returns a copy carrying the signature (the original is not mutated).
func Sign(m *Message, agentID, tenant string) *Message {
	out := *m
	canon, err := CanonicalBytes(&out)
	if err != nil {
		panic("parley: canonical: " + err.Error())
	}
	if tenant == "" {
		tenant = "default"
	}
	sum := sha256.Sum256([]byte(tenant + ":" + agentID))
	priv := ed25519.NewKeyFromSeed(sum[:])
	out.Signature = &Signature{
		Alg: "ed25519",
		Key: "parley-test:" + agentID,
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon)),
	}
	return &out
}

// Verify checks m's signature against the deterministic test key derived
// from the signer id (embedded in signature.key) and the message tenant.
func Verify(m *Message) (bool, string) {
	sig := m.Signature
	if sig == nil {
		return false, "sem assinatura"
	}
	agentID := sig.Key
	if i := strings.Index(sig.Key, ":"); i >= 0 {
		agentID = sig.Key[i+1:]
	}
	tenant := m.Tenant
	if tenant == "" {
		tenant = "default"
	}
	canon, err := CanonicalBytes(m)
	if err != nil {
		return false, "canonical: " + err.Error()
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return false, "assinatura base64 inválida"
	}
	sum := sha256.Sum256([]byte(tenant + ":" + agentID))
	switch sig.Alg {
	case "ed25519":
		pub := ed25519.NewKeyFromSeed(sum[:]).Public().(ed25519.PublicKey)
		if ed25519.Verify(pub, canon, sigBytes) {
			return true, "assinatura válida (ed25519)"
		}
		return false, "assinatura inválida (ed25519)"
	case "hmac-sha256":
		mac := hmac.New(sha256.New, sum[:])
		mac.Write(canon)
		if hmac.Equal(mac.Sum(nil), sigBytes) {
			return true, "assinatura válida (hmac-sha256)"
		}
		return false, "assinatura inválida (hmac)"
	default:
		return false, "algoritmo não suportado: " + sig.Alg
	}
}

// ---------------------------------------------------------------- ids

// AssignID computes the content-addressed message id (idempotency/replay):
// "msg:" + first 16 hex chars of sha256 over the canonical serialization
// of every field except id and signature.
func AssignID(m *Message) {
	tmp := *m
	tmp.ID = ""
	tmp.Signature = nil
	canon, err := CanonicalBytes(&tmp)
	if err != nil {
		panic("parley: canonical: " + err.Error())
	}
	sum := sha256.Sum256(canon)
	m.ID = "msg:" + hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------- path helpers

// ---------------------------------------------------------------- capability body contracts
//
// The schema promises that "body" is validated against the capability's
// input/output contract when present. BodyContract implements that promise:
// the message's target names the capability, and the act selects which side
// of the contract is checked:
//
//	request / inform           -> capability INPUTS  (the caller supplies them)
//	propose / counter /
//	accept / commit            -> capability OUTPUTS (the provider delivers them)
//
// Semantics:
//   - required: every declared input field MUST be present in the body
//     (missing -> violation); outputs are not required (a provider may
//     legitimately omit an optional result — reported, not enforced)
//   - type: declared types are checked when they are primitive (STRING,
//     NUMBER, INT, BOOL, ARRAY, OBJECT); domain types (LOCATION, ROUTE, ...)
//     are opaque — presence (non-null) is all that can be checked
//   - unknown fields in the body are a WARNING (the contract is open
//     world), not a violation
//
// Contract outcomes follow the hybrid-contract rule: REPORTED (warnings),
// never a hard rejection — the counterparty decides.

// ContractKind maps an act to the side of the capability contract it
// engages. Returns "" for acts that carry no body contract.
func ContractKind(act string) string {
	switch act {
	case "request", "inform":
		return "input"
	case "propose", "counter", "accept", "commit":
		return "output"
	}
	return ""
}

// BodyContract validates m.Body against the capability c (the capability
// named by m.Target, from the sender/receiver's card or registry).
func BodyContract(m *Message, c *Capability) map[string]any {
	out := map[string]any{"ok": true, "violations": []string{}, "warnings": []string{}}
	if c == nil || m.Body == nil {
		out["ok"] = true
		out["detail"] = "sem contrato (sem capability declarada ou sem body)"
		return out
	}
	kind := ContractKind(m.Act)
	if kind == "" {
		out["detail"] = "act sem contrato de corpo: " + m.Act
		return out
	}
	fields := c.Inputs
	if kind == "output" {
		fields = c.Outputs
	}
	declared := map[string]bool{}
	for _, f := range fields {
		declared[f.Name] = true
		v, present := m.Body[f.Name]
		switch {
		case !present:
			if kind == "input" {
				out["violations"] = append(out["violations"].([]string),
					"campo obrigatório ausente: "+f.Name)
			} else {
				out["warnings"] = append(out["warnings"].([]string),
					"output declarado ausente: "+f.Name)
			}
		case v == nil:
			out["violations"] = append(out["violations"].([]string),
				"campo nulo: "+f.Name)
		default:
			if reason, bad := checkType(v, f.Type); bad {
				out["violations"] = append(out["violations"].([]string),
					f.Name+": tipo "+f.Type+" esperado, obtido "+reason)
			}
		}
	}
	for k := range m.Body {
		if !declared[k] {
			out["warnings"] = append(out["warnings"].([]string),
				"campo não declarado no contrato: "+k)
		}
	}
	out["capability"] = c.Name
	out["kind"] = kind
	out["ok"] = len(out["violations"].([]string)) == 0
	return out
}

// checkType reports whether v matches the declared (upper-cased) primitive
// type. Domain types are opaque: any non-null value satisfies them.
func checkType(v any, declared string) (string, bool) {
	switch strings.ToUpper(declared) {
	case "STRING":
		if _, ok := v.(string); ok {
			return "string", false
		}
	case "NUMBER":
		if isNumber(v) {
			return "number", false
		}
	case "INT":
		if isIntegralNumber(v) {
			return "int", false
		}
	case "BOOL":
		if _, ok := v.(bool); ok {
			return "bool", false
		}
	case "ARRAY":
		if _, ok := v.([]any); ok {
			return "array", false
		}
		if _, ok := v.([]string); ok {
			return "array", false
		}
	case "OBJECT":
		if _, ok := v.(map[string]any); ok {
			return "object", false
		}
	default:
		// domain type (LOCATION, ROUTE, ...): opaque, presence is enough
		return "", false
	}
	return describeType(v), true
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, int, int64:
		return true
	}
	return false
}

func isIntegralNumber(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == math.Trunc(x)
	case int, int64:
		return true
	}
	return false
}

func describeType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// GetPath resolves a dotted path (e.g. "route.eta_min") in obj.
func GetPath(obj map[string]any, path string) (any, bool) {
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := mm[part]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// pyRepr formats a value the way the Python reference prints it in
// clause details (for readable, stable diagnostics).
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return fmt.Sprintf("%q", x)
	case float64:
		return fmtFloat(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
