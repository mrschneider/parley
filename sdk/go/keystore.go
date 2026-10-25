// PARLEY v0 — KeyStore: real ed25519 keys (production).
//
// The trust model requires signatures with a REAL private key: deriving
// the key from the agent_id (parley.DeriveKey) is TEST mode — anyone who
// knows the id derives the same key and impersonates the agent. The
// KeyStore is the production path: generate (or load) ed25519 pairs,
// expose the public key in the Agent Card (card.Pub), sign messages with
// the private key.
//
// Persistence: one JSON file per agent, chmod 0600, written atomically
// (temp + rename). The file contains the private seed — whoever has the
// file HAS the agent's identity. In real production the file lives on
// encrypted disk / a secret manager; the file format is the same.
//
// Interop: the format is mirrored in Python (parley_keystore.py) — same
// JSON structure, same base64 encoding of the seed.
package parley

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

// KeyEntry is the on-disk JSON for one agent's key pair.
type KeyEntry struct {
	Agent     string `json:"agent"`
	Tenant    string `json:"tenant"`
	Alg       string `json:"alg"` // always "ed25519"
	SeedB64   string `json:"seed_b64"`
	PubKeyB64 string `json:"pub_key_b64"`
	CreatedAt string `json:"created_at"` // RFC3339 UTC
}

// KeyStore maps "tenant|agent" -> ed25519 key pair.
type KeyStore struct {
	entries map[string]ed25519.PrivateKey
}

// NewKeyStore builds an empty keystore.
func NewKeyStore() *KeyStore {
	return &KeyStore{entries: map[string]ed25519.PrivateKey{}}
}

func ksKey(tenant, agent string) string {
	if tenant == "" {
		tenant = "default"
	}
	return tenant + "|" + agent
}

// Generate creates a fresh ed25519 key pair for agent/tenant and stores it.
func (ks *KeyStore) Generate(agent, tenant string) (ed25519.PublicKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("parley: keystore: %w", err)
	}
	ks.entries[ksKey(tenant, agent)] = priv
	return priv.Public().(ed25519.PublicKey), nil
}

// Put stores an existing private key (loaded from external secret material).
func (ks *KeyStore) Put(agent, tenant string, priv ed25519.PrivateKey) {
	ks.entries[ksKey(tenant, agent)] = priv
}

// Get returns the private key for agent/tenant.
func (ks *KeyStore) Get(agent, tenant string) (ed25519.PrivateKey, bool) {
	priv, ok := ks.entries[ksKey(tenant, agent)]
	return priv, ok
}

// MustGet panics when the key is absent (signing without a key is a
// programming error, not a runtime condition).
func (ks *KeyStore) MustGet(agent, tenant string) ed25519.PrivateKey {
	priv, ok := ks.Get(agent, tenant)
	if !ok {
		panic(fmt.Sprintf("parley: keystore: missing key for %s/%s", tenant, agent))
	}
	return priv
}

// PubKey returns the public key for agent/tenant (nil when absent).
func (ks *KeyStore) PubKey(agent, tenant string) (ed25519.PublicKey, bool) {
	priv, ok := ks.Get(agent, tenant)
	if !ok {
		return nil, false
	}
	return priv.Public().(ed25519.PublicKey), true
}

// SignWith signs m with the stored key of m.From.ID (production path:
// SignMessage below). Unlike parley.Sign (test derivation), the signature
// key id is the agent id itself — the verifier resolves the key from the
// signer's CARD (card.Pub), not by derivation.
func (ks *KeyStore) SignWith(m *Message, tenant string) (*Message, error) {
	priv, ok := ks.Get(m.From.ID, tenant)
	if !ok {
		return nil, fmt.Errorf("parley: keystore: missing key for %s/%s", tenant, m.From.ID)
	}
	out := *m
	canon, err := CanonicalBytes(&out)
	if err != nil {
		return nil, fmt.Errorf("parley: canonical: %w", err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	out.Signature = &Signature{
		Alg: "ed25519",
		Key: base64.StdEncoding.EncodeToString(pub), // raw pub key as key id
		Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canon)),
	}
	return &out, nil
}

// Save writes the whole keystore to path with a cross-process lock and an
// atomic rename, permissions 0600.
func (ks *KeyStore) Save(path string) error {
	list := make([]KeyEntry, 0, len(ks.entries))
	for k, priv := range ks.entries {
		tenant, agent := splitKsKey(k)
		pub := priv.Public().(ed25519.PublicKey)
		list = append(list, KeyEntry{
			Agent: agent, Tenant: tenant, Alg: "ed25519",
			SeedB64:   base64.StdEncoding.EncodeToString(priv.Seed()),
			PubKeyB64: base64.StdEncoding.EncodeToString(pub),
		})
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, b, 0600)
}

// Load keystore entries from path (skips missing file).
func (ks *KeyStore) Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []KeyEntry
	if err := json.Unmarshal(b, &list); err != nil {
		return err
	}
	for _, e := range list {
		seed, err := base64.StdEncoding.DecodeString(e.SeedB64)
		if err != nil {
			return fmt.Errorf("parley: keystore: invalid base64 seed (%s): %w", e.Agent, err)
		}
		if len(seed) != ed25519.SeedSize {
			return fmt.Errorf("parley: keystore: seed with invalid size (%s)", e.Agent)
		}
		ks.entries[ksKey(e.Tenant, e.Agent)] = ed25519.NewKeyFromSeed(seed)
	}
	return nil
}

// LoadOrGenerate loads the keystore from path; for every agent in agents
// (tenant fixed) without a key, generates one and persists back. Returns
// the public keys by agent id.
func LoadOrGenerate(path string, tenant string, agents []string) (*KeyStore, map[string]ed25519.PublicKey, error) {
	ks := NewKeyStore()
	if err := ks.Load(path); err != nil {
		return nil, nil, err
	}
	dirty := false
	pubs := map[string]ed25519.PublicKey{}
	for _, a := range agents {
		if _, ok := ks.Get(a, tenant); !ok {
			pub, err := ks.Generate(a, tenant)
			if err != nil {
				return nil, nil, err
			}
			dirty = true
			pubs[a] = pub
			continue
		}
		pub, _ := ks.PubKey(a, tenant)
		pubs[a] = pub
	}
	if dirty {
		if err := ks.Save(path); err != nil {
			return nil, nil, err
		}
	}
	return ks, pubs, nil
}

// SignCardWith re-signs c with an explicit private key (production card
// path — the pub in the card must match priv).
func SignCardWith(c *Card, priv ed25519.PrivateKey) *Card {
	return SignCard(c, priv)
}

func splitKsKey(k string) (tenant, agent string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return "default", k
}
