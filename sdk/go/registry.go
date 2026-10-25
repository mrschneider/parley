// PARLEY v0 — Registro de Agent Cards (descoberta de capacidades).
//
// O registro é apenas um diretório: ele NÃO emite confiança. A confiança
// vem do card assinado (self-contained trust): Publish exige card válido
// (assinatura verificada contra a chave pública declarada no próprio card),
// e Discover retorna cards — o chamador decide se confia (ex.: por
// reputação, tenant, ou uma raiz de confiança externa).
//
// Semântica:
//   - Publish: idempotente para o mesmo card id (re-publicar é no-op);
//     um card novo para o mesmo (tenant, agent) SUPERSEDE o anterior.
//   - Revogação: publicar um card com revoked=true (novo id de conteúdo);
//     Discover filtra cards revogados.
//   - Discover(tenant, "nome.cap", version): cards válidos, não revogados,
//     que oferecem a capability com exatamente essa versão.
package parley

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// VerifyWithKey verifies m's signature against an explicit public key
// (instead of deriving the key from the agent id). This is the real
// transport path: the key comes from the agent's card (card.Pub), so a
// registry entry is the trust anchor, not a shared derivation.
func VerifyWithKey(m *Message, pub ed25519.PublicKey) (bool, string) {
	sig := m.Signature
	if sig == nil {
		return false, "sem assinatura"
	}
	if sig.Alg != "ed25519" {
		return false, "algoritmo não suportado: " + sig.Alg
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return false, "assinatura base64 inválida"
	}
	canon, err := CanonicalBytes(m)
	if err != nil {
		return false, "canonical: " + err.Error()
	}
	if ed25519.Verify(pub, canon, sigBytes) {
		return true, "assinatura válida (ed25519, chave do card)"
	}
	return false, "assinatura inválida (ed25519, chave do card)"
}

// CardEntry is a card as stored in the registry.
type CardEntry struct {
	Card        *Card  `json:"card"`
	Publisher   string `json:"publisher"`    // agent id that published (audit)
	PublishedAt string `json:"published_at"` // RFC3339
}

// Registry is an in-memory directory of agent cards.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*CardEntry // card.ID -> entry
	byAgent map[string]string     // "tenant|agent" -> current card.ID
	byCap   map[string][]string   // "tenant|cap|version" -> card.IDs
}

func NewRegistry() *Registry {
	return &Registry{
		entries: map[string]*CardEntry{},
		byAgent: map[string]string{},
		byCap:   map[string][]string{},
	}
}

func agentKey(tenant, agent string) string { return tenant + "|" + agent }
func capKey(tenant, name string, v int) string {
	return fmt.Sprintf("%s|%s|%d", tenant, name, v)
}

// Publish stores a signed card. The card MUST verify (signature against
// its own declared public key) — the registry never stores unverified
// cards. Re-publishing the same card id is a no-op (idempotent); a new
// card for the same (tenant, agent) supersedes the previous one.
func (r *Registry) Publish(c *Card, publisher string, ts time.Time) error {
	ok, why := VerifyCard(c)
	if !ok {
		return fmt.Errorf("parley: registry: card rejeitado: %s", why)
	}
	if c.ID == "" {
		return fmt.Errorf("parley: registry: card sem id (use SignCard)")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, dup := r.entries[c.ID]; dup {
		_ = prev
		return nil // idempotente
	}
	// supersede: remove old card's capability entries for (tenant, agent)
	if oldID, ok2 := r.byAgent[agentKey(c.Tenant, c.Agent)]; ok2 {
		if old := r.entries[oldID]; old != nil {
			for _, cap := range old.Card.Provides {
				r.removeCap(old.Card.Tenant, cap.Name, cap.Version, oldID)
			}
		}
	}
	r.entries[c.ID] = &CardEntry{
		Card: c, Publisher: publisher,
		PublishedAt: ts.UTC().Format("2006-01-02T15:04:05Z"),
	}
	r.byAgent[agentKey(c.Tenant, c.Agent)] = c.ID
	for _, cap := range c.Provides {
		k := capKey(c.Tenant, cap.Name, cap.Version)
		r.byCap[k] = append(r.byCap[k], c.ID)
	}
	return nil
}

func (r *Registry) removeCap(tenant, name string, v int, id string) {
	k := capKey(tenant, name, v)
	list := r.byCap[k]
	out := list[:0]
	for _, x := range list {
		if x != id {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		delete(r.byCap, k)
	} else {
		r.byCap[k] = out
	}
}

// Get returns the current (non-superseded) card for (tenant, agent).
func (r *Registry) Get(tenant, agent string) (*Card, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byAgent[agentKey(tenant, agent)]
	if !ok {
		return nil, false
	}
	e, ok := r.entries[id]
	if !ok {
		return nil, false
	}
	return e.Card, true
}

// VerifyMessageWithCard resolves the signer's current registered card,
// decodes card.Pub, and verifies the message against that key. This is
// the production trust path: the registry entry is the trust anchor. It
// rejects revoked cards and test-derived signatures that do not match the
// registered key.
func (r *Registry) VerifyMessageWithCard(m *Message) (bool, string) {
	tenant := m.Tenant
	if tenant == "" {
		tenant = "default"
	}
	card, ok := r.Get(tenant, m.From.ID)
	if !ok {
		return false, "card não registrado: " + tenant + "/" + m.From.ID
	}
	if card.Revoked {
		return false, "card revogado: " + tenant + "/" + m.From.ID
	}
	rawPub, err := base64.StdEncoding.DecodeString(card.Pub.Key)
	if err != nil {
		return false, "card: chave pública base64 inválida"
	}
	if len(rawPub) != ed25519.PublicKeySize {
		return false, "card: chave pública inválida"
	}
	return VerifyWithKey(m, ed25519.PublicKey(rawPub))
}

// Discover returns cards (tenant-scoped, verified, not revoked) that offer
// capability name at exactly version, sorted by agent id for determinism.
func (r *Registry) Discover(tenant, name string, version int) []*Card {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := append([]string(nil), r.byCap[capKey(tenant, name, version)]...)
	sort.Strings(ids)
	var out []*Card
	for _, id := range ids {
		e, ok := r.entries[id]
		if !ok {
			continue
		}
		// skip superseded cards: if the agent's current card is another one,
		// this entry is stale.
		if cur, ok2 := r.byAgent[agentKey(e.Card.Tenant, e.Card.Agent)]; ok2 && cur != id {
			continue
		}
		if e.Card.Revoked {
			continue
		}
		out = append(out, e.Card)
	}
	return out
}

// List returns all current cards for a tenant (deterministic order).
func (r *Registry) List(tenant string) []*Card {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Card
	for _, id := range r.byAgent {
		e, ok := r.entries[id]
		if !ok || e.Card.Tenant != tenant {
			continue
		}
		out = append(out, e.Card)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// Marshal serializes the registry to canonical JSON (persistence / HTTP
// registry).
func (r *Registry) Marshal() ([]byte, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entries := make([]any, 0, len(r.entries))
	for _, e := range r.entries {
		entries = append(entries, []any{
			CardToAny(e.Card),
			e.Publisher,
			e.PublishedAt,
		})
	}
	return marshalCanonical(entries)
}

// Unmarshal loads a registry from canonical JSON (mirror of Marshal).
func (r *Registry) Unmarshal(b []byte) error {
	var list []json.RawMessage
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	fresh := NewRegistry()
	for _, item := range list {
		var triple []json.RawMessage
		if err := json.Unmarshal(item, &triple); err != nil {
			return err
		}
		if len(triple) != 3 {
			return fmt.Errorf("parley: registry: entrada inválida")
		}
		var c Card
		if err := json.Unmarshal(triple[0], &c); err != nil {
			return err
		}
		var publisher string
		if err := json.Unmarshal(triple[1], &publisher); err != nil {
			return err
		}
		var publishedAt string
		if err := json.Unmarshal(triple[2], &publishedAt); err != nil {
			return err
		}
		ts, err := time.Parse("2006-01-02T15:04:05Z", publishedAt)
		if err != nil {
			ts = time.Now()
		}
		if err := fresh.Publish(&c, publisher, ts); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries, r.byAgent, r.byCap = fresh.entries, fresh.byAgent, fresh.byCap
	return nil
}

// Save atomically persists the registry to path (temp + rename, 0644).
func (r *Registry) Save(path string) error {
	b, err := r.Marshal()
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, b, 0644)
}

// Load restores a registry from path (skips a missing file).
func (r *Registry) Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return r.Unmarshal(b)
}
