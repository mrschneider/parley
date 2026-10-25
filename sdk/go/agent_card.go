// PARLEY v0 — Agent Card (registro/descoberta de capacidades).
//
// O Agent Card é o "who am I" de um agente: identidade, tenant, endpoint,
// capacidades oferecidas e a chave pública ed25519 usada para verificar as
// assinaturas das mensagens. O card é ASSINADO (self-contained trust): quem
// publica no registro não precisa ser confiável — o verificador confia na
// chave que ele mesmo declarou e usa essa chave para verificar o card e,
// em seguida, todas as mensagens do agente.
package parley

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Card is the PARLEY v0 agent card. It is a content-addressed, signed
// document: id = "card:" + first 16 hex chars of sha256 over the canonical
// serialization of every field except id and signature (same rule as
// messages — a card that changes content changes id).
type Card struct {
	V         int              `json:"v"`
	ID        string           `json:"id"`
	Agent     string           `json:"agent"`     // stable agent id (matches message "from.id")
	Tenant    string           `json:"tenant"`    // tenant key the card is scoped to
	Endpoint  string           `json:"endpoint"`  // transport endpoint (http url / queue name)
	Transport string           `json:"transport"` // "http" | "queue" | "ws"
	Provides  []CardCapability `json:"provides"`  // capabilities offered
	Revoked   bool             `json:"revoked,omitempty"`
	Pub       CardPubKey       `json:"pub"` // public key to verify this agent's messages
	Signature *CardSignature   `json:"signature,omitempty"`
}

// CardCapability is one offered capability (summary view of the .parley
// 'provides' declaration).
type CardCapability struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Intent  string `json:"intent,omitempty"`
}

// CardPubKey carries the public key in base64 (raw 32 bytes for ed25519).
type CardPubKey struct {
	Alg string `json:"alg"`
	Key string `json:"key"` // base64 raw public key
}

// CardSignature is the signature over the canonical card (all fields
// except id and signature).
type CardSignature struct {
	Alg string `json:"alg"`
	Sig string `json:"sig"` // base64
}

// CardToAny builds the plain-value tree of c preserving int/float
// distinctness (canonical number rule).
func CardToAny(c *Card) map[string]any {
	out := map[string]any{
		"v":         int64(c.V),
		"agent":     c.Agent,
		"tenant":    c.Tenant,
		"endpoint":  c.Endpoint,
		"transport": c.Transport,
		"pub":       map[string]any{"alg": c.Pub.Alg, "key": c.Pub.Key},
	}
	if c.ID != "" {
		out["id"] = c.ID
	}
	if len(c.Provides) > 0 {
		arr := make([]any, 0, len(c.Provides))
		for _, p := range c.Provides {
			pc := map[string]any{"name": p.Name, "version": int64(p.Version)}
			if p.Intent != "" {
				pc["intent"] = p.Intent
			}
			arr = append(arr, pc)
		}
		out["provides"] = arr
	}
	if c.Revoked {
		out["revoked"] = true
	}
	if c.Signature != nil {
		out["signature"] = map[string]any{"alg": c.Signature.Alg, "sig": c.Signature.Sig}
	}
	return out
}

// CardCanonicalBytes returns the canonical serialization of c: every field
// except id and signature, keys sorted, compact, canonical number rule.
func CardCanonicalBytes(c *Card) ([]byte, error) {
	tree := CardToAny(c)
	delete(tree, "signature")
	delete(tree, "id")
	return marshalCanonical(tree)
}

// SignCard signs c with the agent's ed25519 private key and assigns the
// content-addressed card id. Returns a copy (the original is not mutated).
func SignCard(c *Card, priv ed25519.PrivateKey) *Card {
	out := *c
	out.Signature = nil
	out.ID = ""
	canon, err := CardCanonicalBytes(&out)
	if err != nil {
		panic("parley: card canonical: " + err.Error())
	}
	out.Signature = &CardSignature{
		Alg: "ed25519",
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon)),
	}
	sum := sha256.Sum256(canon)
	out.ID = "card:" + hex.EncodeToString(sum[:])[:16]
	return &out
}

// VerifyCard checks c's signature against the public key declared in c.Pub.
// This is the trust anchor: a valid card proves the holder of the matching
// private key published it, and c.Pub can then be used to verify every
// message from c.Agent.
func VerifyCard(c *Card) (bool, string) {
	sig := c.Signature
	if sig == nil {
		return false, "sem assinatura"
	}
	if sig.Alg != "ed25519" {
		return false, "algoritmo não suportado: " + sig.Alg
	}
	pub, err := base64.StdEncoding.DecodeString(c.Pub.Key)
	if err != nil {
		return false, "chave pública base64 inválida"
	}
	if len(pub) != ed25519.PublicKeySize {
		return false, fmt.Sprintf("tamanho da chave pública inválido: %d", len(pub))
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return false, "assinatura base64 inválida"
	}
	tmp := *c
	tmp.Signature = nil
	tmp.ID = ""
	canon, err := CardCanonicalBytes(&tmp)
	if err != nil {
		return false, "canonical: " + err.Error()
	}
	if ed25519.Verify(ed25519.PublicKey(pub), canon, sigBytes) {
		return true, "assinatura válida (ed25519)"
	}
	return false, "assinatura inválida (ed25519)"
}

// CardID recomputes the content-addressed card id from c (ignoring any
// existing id/signature).
func CardID(c *Card) string {
	tmp := *c
	tmp.Signature = nil
	tmp.ID = ""
	canon, err := CardCanonicalBytes(&tmp)
	if err != nil {
		panic("parley: card canonical: " + err.Error())
	}
	sum := sha256.Sum256(canon)
	return "card:" + hex.EncodeToString(sum[:])[:16]
}

// NewCard builds a card for agentID/tenant from a .parley Doc's capability
// declarations (the "provides" section), ready for SignCard.
func NewCard(agentID, tenant, endpoint, transport string, doc *Doc) *Card {
	var provides []CardCapability
	for _, ag := range doc.Agents {
		if ag.ID == agentID {
			for _, p := range ag.Provides {
				cap, ok := doc.Capabilities[p]
				if !ok {
					continue
				}
				provides = append(provides, CardCapability{
					Name: cap.Name, Version: cap.Version,
				})
			}
		}
	}
	// deterministic order for stable ids
	for i := 1; i < len(provides); i++ {
		for j := i; j > 0 && provides[j].Name < provides[j-1].Name; j-- {
			provides[j-1], provides[j] = provides[j], provides[j-1]
		}
	}
	return &Card{
		V:         Version,
		Agent:     agentID,
		Tenant:    tenant,
		Endpoint:  endpoint,
		Transport: transport,
		Provides:  provides,
	}
}
