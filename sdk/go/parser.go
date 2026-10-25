package parley

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Now is the clock used by the parser for message timestamps. Override in
// tests (or embed a custom clock) for reproducible content-addressed ids.
var Now = time.Now

// Doc is a parsed .parley document.
type Doc struct {
	Agents       map[string]Agent      // alias -> agent
	Capabilities map[string]Capability // capability name -> declaration
	Messages     []*Message            // in declaration order
}

// Agent is one declared agent (alias -> stable id + provided capabilities).
type Agent struct {
	ID       string   `json:"id"`
	Alias    string   `json:"alias"`
	Provides []string `json:"provides"`
}

// Capability is a declared capability (inputs/outputs/cost/confidence).
type Capability struct {
	Name              string   `json:"name"`
	Version           int      `json:"version"`
	Inputs            []Field  `json:"inputs"`
	Outputs           []Field  `json:"outputs"`
	Cost              []Check  `json:"cost"`
	DefaultConfidence *float64 `json:"default_confidence"`
	By                string   `json:"by"`
}

var (
	agentRe  = regexp.MustCompile(`(?i)^AGENT\s+"([^"]+)"\s+AS\s+([A-Za-z_][\w-]*)$`)
	provRe   = regexp.MustCompile(`(?i)^PROVIDES\s+"?([a-z0-9_.]+)"?\s+V(\d+)$`)
	ioInRe   = regexp.MustCompile(`(?i)^INPUT\s+(\w+)\s*:\s*(\w+)$`)
	ioOutRe  = regexp.MustCompile(`(?i)^OUTPUT\s+(\w+)\s*:\s*(\w+)$`)
	costRe   = regexp.MustCompile(`(?i)^COST\s+(\w+)\s*(<=|>=|<|>|==)\s*(.+)$`)
	headerRe = regexp.MustCompile(`^([A-Za-z_][\w-]*)\s*->\s*([A-Za-z_][\w-]*)$`)
	expectRe = regexp.MustCompile(`^([A-Za-z_][\w.]*)\s*(<=|>=|<|>|==|in|not_null|~)\s*(.+)$`)
	weightRe = regexp.MustCompile(`(?i)\s+W\s+(\d+(?:\.\d+)?)\s*$`)
	intRe    = regexp.MustCompile(`^-?\d+$`)
	floatRe  = regexp.MustCompile(`^-?\d+\.\d+$`)
)

var ops = map[string]string{
	"<": "lt", "<=": "lte", ">": "gt", ">=": "gte", "==": "eq", "=": "eq",
	"in": "in", "not_null": "not_null", "~": "regex",
}

// ParseText parses .parley source into a Doc (same grammar as the Python
// reference parser). It does NOT assign message ids — call Finalize.
func ParseText(src string) (*Doc, error) {
	doc := &Doc{
		Agents:       map[string]Agent{},
		Capabilities: map[string]Capability{},
	}
	curAgent := ""
	curProv := ""
	var curMsg *Message

	for ln, raw := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if i := strings.Index(raw, "#"); i >= 0 {
			raw = raw[:i]
		}
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		first := s
		if sp := strings.IndexAny(s, " \t"); sp >= 0 {
			first = s[:sp]
		}
		upper := strings.ToUpper(first)

		// ---- capability declarations ---------------------------------------
		switch upper {
		case "AGENT":
			m := agentRe.FindStringSubmatch(s)
			if m == nil {
				return nil, fmt.Errorf("ln %d: malformed AGENT: %s", ln+1, s)
			}
			curAgent = m[2]
			doc.Agents[curAgent] = Agent{ID: m[1], Alias: curAgent}
			continue
		case "PROVIDES":
			m := provRe.FindStringSubmatch(s)
			if m == nil || curAgent == "" {
				return nil, fmt.Errorf("ln %d: malformed PROVIDES: %s", ln+1, s)
			}
			curProv = m[1]
			ver, _ := strconv.Atoi(m[2])
			if _, ok := doc.Capabilities[curProv]; !ok {
				doc.Capabilities[curProv] = Capability{Name: curProv, By: curAgent}
			}
			cap := doc.Capabilities[curProv]
			cap.Version = ver
			doc.Capabilities[curProv] = cap
			ag := doc.Agents[curAgent]
			ag.Provides = append(ag.Provides, curProv)
			doc.Agents[curAgent] = ag
			continue
		case "INPUT", "OUTPUT":
			var m []string
			if upper == "INPUT" {
				m = ioInRe.FindStringSubmatch(s)
			} else {
				m = ioOutRe.FindStringSubmatch(s)
			}
			if m == nil || curProv == "" {
				return nil, fmt.Errorf("ln %d: malformed INPUT/OUTPUT: %s", ln+1, s)
			}
			cap := doc.Capabilities[curProv]
			f := Field{Name: m[1], Type: strings.ToUpper(m[2])}
			if upper == "INPUT" {
				cap.Inputs = append(cap.Inputs, f)
			} else {
				cap.Outputs = append(cap.Outputs, f)
			}
			doc.Capabilities[curProv] = cap
			continue
		case "COST":
			m := costRe.FindStringSubmatch(s)
			if m == nil || curProv == "" {
				return nil, fmt.Errorf("ln %d: malformed COST: %s", ln+1, s)
			}
			v, err := parseValue(m[3])
			if err != nil {
				return nil, fmt.Errorf("ln %d: COST value: %w", ln+1, err)
			}
			cap := doc.Capabilities[curProv]
			cap.Cost = append(cap.Cost, Check{Field: m[1], Op: ops[m[2]], Value: v})
			doc.Capabilities[curProv] = cap
			continue
		case "DEFAULT_CONFIDENCE":
			if curProv == "" {
				return nil, fmt.Errorf("ln %d: DEFAULT_CONFIDENCE outside PROVIDES", ln+1)
			}
			v, err := parseValue(strings.TrimSpace(s[len("DEFAULT_CONFIDENCE"):]))
			if err != nil {
				return nil, fmt.Errorf("ln %d: DEFAULT_CONFIDENCE: %w", ln+1, err)
			}
			if f, ok := v.(float64); ok {
				cap := doc.Capabilities[curProv]
				cap.DefaultConfidence = &f
				doc.Capabilities[curProv] = cap
			}
			continue
		}

		// ---- messages ---------------------------------------------------------
		if hm := headerRe.FindStringSubmatch(s); hm != nil {
			if _, ok := doc.Agents[hm[1]]; !ok {
				return nil, fmt.Errorf("ln %d: unknown agent: %s", ln+1, hm[1])
			}
			if _, ok := doc.Agents[hm[2]]; !ok {
				return nil, fmt.Errorf("ln %d: unknown agent: %s", ln+1, hm[2])
			}
			curMsg = NewMessage(doc.Agents[hm[1]].ID, doc.Agents[hm[2]].ID, Now())
			doc.Messages = append(doc.Messages, curMsg)
			continue
		}
		if curMsg == nil {
			return nil, fmt.Errorf("ln %d: line outside context (agent/message): %s", ln+1, s)
		}
		m := curMsg

		switch {
		case isAct(upper):
			m.Act = strings.ToLower(upper)
			if rest := restOf(s); rest != "" {
				m.Target = strings.Trim(rest, `"`)
			}
		case upper == "INTENT":
			v, err := parseValue(restOf(s))
			if err != nil {
				return nil, fmt.Errorf("ln %d: INTENT: %w", ln+1, err)
			}
			if str, ok := v.(string); ok {
				m.Intent = str
			} else {
				m.Intent = fmt.Sprintf("%v", v)
			}
		case upper == "WITH":
			if m.Body == nil {
				m.Body = map[string]any{}
			}
			for _, pair := range splitTop(restOf(s)) {
				k, v, ok := strings.Cut(pair, "=")
				if !ok {
					return nil, fmt.Errorf("ln %d: WITH pair without '=': %s", ln+1, pair)
				}
				pv, err := parseValue(strings.TrimSpace(v))
				if err != nil {
					return nil, fmt.Errorf("ln %d: WITH: %w", ln+1, err)
				}
				m.Body[strings.TrimSpace(k)] = pv
			}
		case upper == "BY":
			m.Deadline = strings.TrimSpace(restOf(s))
		case upper == "NOTE":
			v, err := parseValue(restOf(s))
			if err != nil {
				return nil, fmt.Errorf("ln %d: NOTE: %w", ln+1, err)
			}
			if str, ok := v.(string); ok {
				m.Note = str
			} else {
				m.Note = fmt.Sprintf("%v", v)
			}
		case upper == "CONFIDENCE":
			v, err := parseValue(restOf(s))
			if err != nil {
				return nil, fmt.Errorf("ln %d: CONFIDENCE: %w", ln+1, err)
			}
			if f, ok := v.(float64); ok {
				m.Confidence = &f
			}
		case upper == "TENANT":
			v, err := parseValue(restOf(s))
			if err != nil {
				return nil, fmt.Errorf("ln %d: TENANT: %w", ln+1, err)
			}
			if str, ok := v.(string); ok {
				m.Tenant = str
			}
		case upper == "PARENT":
			m.Parent = strings.Trim(strings.TrimSpace(restOf(s)), `"`)
		case upper == "PROVENANCE":
			for _, p := range splitTop(restOf(s)) {
				m.Provenance = append(m.Provenance, strings.Trim(p, `"`))
			}
		case upper == "FALLBACK":
			rest, _, ok := strings.Cut(s, "->")
			_ = rest
			if !ok {
				return nil, fmt.Errorf("ln %d: FALLBACK without '->': %s", ln+1, s)
			}
			tail := strings.TrimSpace(rest)
			parts := strings.Fields(tail)
			if len(parts) == 0 {
				return nil, fmt.Errorf("ln %d: FALLBACK without target: %s", ln+1, s)
			}
			fb := &Fallback{To: AgentRef{ID: strings.Trim(parts[0], `"`)}}
			if len(parts) >= 2 && strings.HasPrefix(parts[1], `"`) {
				rv, err := parseValue(strings.Join(parts[1:], " "))
				if err == nil {
					if str, ok := rv.(string); ok {
						fb.Reason = str
					}
				}
			}
			m.Fallback = fb
		case upper == "EXPECT":
			// s starts with a case-variant of "EXPECT" (6 chars)
			rest := strings.TrimSpace(s[len("EXPECT"):])
			weight := 1.0
			if wm := weightRe.FindStringSubmatchIndex(rest); wm != nil {
				w, err := strconv.ParseFloat(rest[wm[2]:wm[3]], 64)
				if err == nil {
					weight = w
					rest = strings.TrimSpace(rest[:wm[0]])
				}
			}
			cl := Clause{Weight: weight}
			if strings.HasPrefix(strings.ToUpper(rest), "NL ") {
				v, err := parseValue(rest[3:])
				if err != nil {
					return nil, fmt.Errorf("ln %d: EXPECT nl: %w", ln+1, err)
				}
				if str, ok := v.(string); ok {
					cl.NL = str
				} else {
					cl.NL = fmt.Sprintf("%v", v)
				}
			} else if cm := expectRe.FindStringSubmatch(rest); cm != nil {
				v, err := parseValue(cm[3])
				if err != nil {
					return nil, fmt.Errorf("ln %d: EXPECT: %w", ln+1, err)
				}
				cl.Check = &Check{Field: cm[1], Op: ops[cm[2]], Value: v}
			} else {
				return nil, fmt.Errorf("ln %d: malformed EXPECT: %s", ln+1, s)
			}
			m.Expect = append(m.Expect, cl)
		case upper == "CLAUSES":
			for _, p := range splitTop(restOf(s)) {
				m.Clauses = append(m.Clauses, strings.Trim(p, `"`))
			}
		default:
			return nil, fmt.Errorf("ln %d: unknown token: %q in %s", ln+1, first, s)
		}
	}

	// stable clause ids
	for _, m := range doc.Messages {
		for i := range m.Expect {
			if m.Expect[i].ID == "" {
				m.Expect[i].ID = fmt.Sprintf("c%d", i+1)
			}
		}
	}
	return doc, nil
}

// Finalize assigns content-addressed ids and THEN resolves @msgN
// references (parent/provenance correlation, human-readable) — the same
// order as the Python reference, so ids are byte-identical across
// platforms.
func Finalize(doc *Doc) []*Message {
	for _, m := range doc.Messages {
		AssignID(m)
	}
	for _, m := range doc.Messages {
		if strings.HasPrefix(m.Parent, "@msg") {
			m.Parent = doc.Messages[mustMsgN(m.Parent)-1].ID
		}
		for i, p := range m.Provenance {
			if strings.HasPrefix(p, "@msg") {
				m.Provenance[i] = doc.Messages[mustMsgN(p)-1].ID
			}
		}
	}
	return doc.Messages
}

func mustMsgN(ref string) int {
	n, err := strconv.Atoi(ref[4:])
	if err != nil || n < 1 {
		panic("parley: invalid @msg reference: " + ref)
	}
	return n
}

func isAct(upper string) bool {
	for _, a := range Acts {
		if strings.ToUpper(a) == upper {
			return true
		}
	}
	return false
}

// restOf returns everything after the first whitespace token of s.
func restOf(s string) string {
	rest := strings.TrimSpace(s)
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		return rest[i+1:]
	}
	return ""
}

// splitTop splits s on sep at depth 0, respecting []{} nesting and strings.
func splitTop(s string) []string {
	var parts, cur []string
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			inStr = !inStr
		}
		if !inStr {
			switch ch {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
		if ch == ',' && depth == 0 && !inStr {
			parts = append(parts, strings.TrimSpace(strings.Join(cur, "")))
			cur = cur[:0]
		} else {
			cur = append(cur, string(ch))
		}
	}
	if t := strings.TrimSpace(strings.Join(cur, "")); t != "" {
		parts = append(parts, t)
	}
	return parts
}

// parseValue parses a scalar/list/object literal: JSON strings, JSON
// objects/arrays, booleans, integers, floats, bare words.
func parseValue(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty value")
	}
	if s[0] == '"' {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("string: %w", err)
		}
		return v, nil
	}
	if s[0] == '[' || s[0] == '{' {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v, nil
		}
		if s[0] == '[' { // loose list: [a, b, c]
			inner := strings.TrimSpace(s[1 : len(s)-1])
			if inner == "" {
				return []any{}, nil
			}
			out := make([]any, 0)
			for _, p := range splitTop(inner) {
				v, err := parseValue(p)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
		return nil, fmt.Errorf("invalid JSON: %s", s)
	}
	switch strings.ToLower(s) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if intRe.MatchString(s) {
		n, _ := strconv.ParseInt(s, 10, 64)
		return float64(n), nil // JSON has one number type
	}
	if floatRe.MatchString(s) {
		f, _ := strconv.ParseFloat(s, 64)
		return f, nil
	}
	return s, nil
}
