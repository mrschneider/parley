// PARLEY v0 — Agreement: the settlement receipt of a negotiation.
//
// An accept and a commit both settle a thread, but they are not the same
// business fact: commit asserts fulfillment/intent to perform, accept
// asserts agreement to the offered terms. The Agreement materializes the
// final outcome as a canonical, content-addressed, signed artifact: who
// agreed, which offer won, which clauses became binding, and the body the
// provider committed to. It is the answer to the audit question "what was
// agreed?" without replaying the whole thread.
package parley

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
)

// AgreementSignature binds one participant to the agreement bytes.
type AgreementSignature struct {
	Agent     string     `json:"agent"`
	Signature *Signature `json:"signature"`
}

// Agreement is the canonical settlement receipt.
type Agreement struct {
	V              int                  `json:"v"`
	ID             string               `json:"id,omitempty"`
	RequestID      string               `json:"request_id"`
	OfferID        string               `json:"offer_id"`
	AcceptanceID   string               `json:"acceptance_id"`
	Act            string               `json:"act"` // accept | commit
	Tenant         string               `json:"tenant,omitempty"`
	Requestor      AgentRef             `json:"requestor"`
	Provider       AgentRef             `json:"provider"`
	Target         string               `json:"target,omitempty"`
	Body           map[string]any       `json:"body,omitempty"`
	BindingClauses []string             `json:"binding_clauses"`
	Signatures     []AgreementSignature `json:"signatures,omitempty"`
}

// BuildAgreement materializes the settlement from the request, the winning
// offer, and the acceptance. Binding NL clauses are exactly those the
// acceptance listed; the offer body is the agreed body.
func BuildAgreement(request, offer, acceptance *Message) (*Agreement, error) {
	if request == nil || offer == nil || acceptance == nil {
		return nil, fmt.Errorf("parley: agreement: request/offer/acceptance required")
	}
	if request.ID == "" || offer.ID == "" || acceptance.ID == "" {
		return nil, fmt.Errorf("parley: agreement: messages without content id")
	}
	if request.Act != "request" {
		return nil, fmt.Errorf("parley: agreement: root must be request, got %s", request.Act)
	}
	if offer.Act != "propose" && offer.Act != "counter" {
		return nil, fmt.Errorf("parley: agreement: offer must be propose/counter, got %s", offer.Act)
	}
	if acceptance.Act != "accept" && acceptance.Act != "commit" {
		return nil, fmt.Errorf("parley: agreement: acceptance must be accept/commit, got %s", acceptance.Act)
	}
	belongs := acceptance.Parent == request.ID || acceptance.Parent == offer.ID
	if !belongs {
		// provenance also expresses membership (accept without an explicit parent)
		prov := map[string]bool{}
		for _, id := range acceptance.Provenance {
			prov[id] = true
		}
		belongs = prov[request.ID] && prov[offer.ID]
	}
	if !belongs {
		return nil, fmt.Errorf("parley: agreement: acceptance %s does not belong to the request/offer thread", acceptance.ID)
	}
	binding := append([]string(nil), acceptance.Clauses...)
	sort.Strings(binding)
	tenant := acceptance.Tenant
	if tenant == "" {
		tenant = request.Tenant
	}
	body := map[string]any{}
	for k, v := range offer.Body {
		body[k] = v
	}
	return &Agreement{
		V:              Version,
		RequestID:      request.ID,
		OfferID:        offer.ID,
		AcceptanceID:   acceptance.ID,
		Act:            acceptance.Act,
		Tenant:         tenant,
		Requestor:      request.From,
		Provider:       offer.From,
		Target:         request.Target,
		Body:           body,
		BindingClauses: binding,
	}, nil
}

// AgreementToAny builds the canonical value tree. ID and signatures are
// omitted from the signed view.
func AgreementToAny(a *Agreement) map[string]any {
	out := map[string]any{
		"v":             int64(a.V),
		"request_id":    a.RequestID,
		"offer_id":      a.OfferID,
		"acceptance_id": a.AcceptanceID,
		"act":           a.Act,
		"requestor":     agentRefToAny(a.Requestor),
		"provider":      agentRefToAny(a.Provider),
	}
	if a.ID != "" {
		out["id"] = a.ID
	}
	if a.Tenant != "" {
		out["tenant"] = a.Tenant
	}
	if a.Target != "" {
		out["target"] = a.Target
	}
	if a.Body != nil {
		out["body"] = a.Body
	}
	out["binding_clauses"] = stringsToAny(a.BindingClauses)
	if len(a.Signatures) > 0 {
		arr := make([]any, 0, len(a.Signatures))
		for _, s := range a.Signatures {
			arr = append(arr, map[string]any{
				"agent": s.Agent,
				"signature": map[string]any{
					"alg": s.Signature.Alg, "key": s.Signature.Key, "sig": s.Signature.Sig,
				},
			})
		}
		out["signatures"] = arr
	}
	return out
}

// AgreementCanonicalBytes returns the canonical bytes covering every field
// except id and signatures.
func AgreementCanonicalBytes(a *Agreement) ([]byte, error) {
	tree := AgreementToAny(a)
	delete(tree, "id")
	delete(tree, "signatures")
	return marshalCanonical(tree)
}

// AssignAgreementID computes the content-addressed receipt id.
func AssignAgreementID(a *Agreement) error {
	a.ID = ""
	canon, err := AgreementCanonicalBytes(a)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canon)
	a.ID = "agreement:" + hex.EncodeToString(sum[:])[:16]
	return nil
}

// SignAgreement appends (or replaces) agent's signature and refreshes the
// content id. The signer is normally requestor and provider — a receipt
// signed by both is the strongest audit artifact.
func SignAgreement(a *Agreement, agent, tenant string, priv ed25519.PrivateKey) (*Agreement, error) {
	out := *a
	canon, err := AgreementCanonicalBytes(&out)
	if err != nil {
		return nil, err
	}
	sig := &Signature{
		Alg: "ed25519",
		Key: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)),
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon)),
	}
	sigs := make([]AgreementSignature, 0, len(out.Signatures)+1)
	for _, s := range out.Signatures {
		if s.Agent != agent {
			sigs = append(sigs, s)
		}
	}
	sigs = append(sigs, AgreementSignature{Agent: agent, Signature: sig})
	sort.Slice(sigs, func(i, j int) bool { return sigs[i].Agent < sigs[j].Agent })
	out.Signatures = sigs
	if err := AssignAgreementID(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyAgreement verifies every signature over the canonical receipt.
func VerifyAgreement(a *Agreement) (bool, string) {
	if a.ID == "" {
		return false, "agreement without id"
	}
	expected, err := AssignAgreementIDTemp(a)
	if err != nil {
		return false, err.Error()
	}
	if expected.id != a.ID {
		return false, "content id mismatch"
	}
	if len(a.Signatures) == 0 {
		return false, "agreement without signatures"
	}
	seen := map[string]bool{}
	for _, s := range a.Signatures {
		if s.Signature == nil {
			return false, "missing signature: " + s.Agent
		}
		if seen[s.Agent] {
			return false, "duplicate signature: " + s.Agent
		}
		seen[s.Agent] = true
		rawPub, decodeErr := base64.StdEncoding.DecodeString(s.Signature.Key)
		var pub ed25519.PublicKey
		if decodeErr == nil && len(rawPub) == ed25519.PublicKeySize {
			pub = ed25519.PublicKey(rawPub)
		} else {
			// Test/legacy key id support: "parley-test:<agent>".
			agent := s.Signature.Key
			if idx := indexOfByte(s.Signature.Key, ':'); idx >= 0 {
				agent = s.Signature.Key[idx+1:]
			}
			tenant := a.Tenant
			if tenant == "" {
				tenant = "default"
			}
			pub = derivePublic(agent, tenant)
		}
		sigBytes, err := base64.StdEncoding.DecodeString(s.Signature.Sig)
		if err != nil {
			return false, "invalid base64 signature: " + s.Agent
		}
		if !ed25519.Verify(pub, expected.canon, sigBytes) {
			return false, "invalid signature: " + s.Agent
		}
	}
	return true, "agreement valid"
}

type agreementDigest struct {
	id    string
	canon []byte
}

func AssignAgreementIDTemp(a *Agreement) (agreementDigest, error) {
	tmp := *a
	tmp.ID = ""
	canon, err := AgreementCanonicalBytes(&tmp)
	if err != nil {
		return agreementDigest{}, err
	}
	sum := sha256.Sum256(canon)
	return agreementDigest{id: "agreement:" + hex.EncodeToString(sum[:])[:16], canon: canon}, nil
}

func derivePublic(agentID, tenant string) ed25519.PublicKey {
	if tenant == "" {
		tenant = "default"
	}
	sum := sha256.Sum256([]byte(tenant + ":" + agentID))
	priv := ed25519.NewKeyFromSeed(sum[:])
	return priv.Public().(ed25519.PublicKey)
}

func indexOfByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
