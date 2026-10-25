package transport

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	parley "example.org/parley-go"
)

func pubKeyField(t *testing.T, key ed25519.PublicKey, ok bool) parley.CardPubKey {
	t.Helper()
	if !ok {
		t.Fatal("missing public key")
	}
	return parley.CardPubKey{
		Alg: "ed25519",
		Key: base64.StdEncoding.EncodeToString(key),
	}
}

func TestInboxSaveUsesAtomicWrite(t *testing.T) {
	path := t.TempDir() + "/inbox.json"
	in := NewInbox(WithStoreFile(path))
	if err := in.Save(); err != nil {
		t.Fatal(err)
	}
	if err := in.Load(); err != nil {
		t.Fatal(err)
	}
	if in.Len() != 0 {
		t.Fatalf("empty inbox restored %d messages", in.Len())
	}
}

func TestRegistrySaveLoadRoundTrip(t *testing.T) {
	path := t.TempDir() + "/registry.json"
	ks := parley.NewKeyStore()
	agents := []string{"provider-a", "provider-b"}
	for _, agent := range agents {
		if _, err := ks.Generate(agent, "acme"); err != nil {
			t.Fatal(err)
		}
	}
	capability := parley.CardCapability{Name: "route.plan", Version: 1}

	pubA, okA := ks.PubKey("provider-a", "acme")
	cardA := &parley.Card{
		V: 0, Agent: "provider-a", Tenant: "acme", Endpoint: "http://a", Transport: "http",
		Provides: []parley.CardCapability{capability},
		Pub:      pubKeyField(t, pubA, okA),
	}
	signedA := parley.SignCardWith(cardA, ks.MustGet("provider-a", "acme"))

	pubB, okB := ks.PubKey("provider-b", "acme")
	cardB := &parley.Card{
		V: 0, Agent: "provider-b", Tenant: "acme", Endpoint: "http://b", Transport: "http",
		Provides: []parley.CardCapability{capability},
		Pub:      pubKeyField(t, pubB, okB),
	}
	signedB := parley.SignCardWith(cardB, ks.MustGet("provider-b", "acme"))

	registry := parley.NewRegistry()
	if err := registry.Publish(signedA, "provider-a", t0); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(signedB, "provider-b", t0); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(path); err != nil {
		t.Fatal(err)
	}

	restored := parley.NewRegistry()
	if err := restored.Load(path); err != nil {
		t.Fatal(err)
	}
	cards := restored.List("acme")
	if len(cards) != 2 {
		t.Fatalf("restored %d cards, want 2", len(cards))
	}
	found := map[string]bool{}
	for _, card := range cards {
		found[card.Agent] = true
		if ok, why := parley.VerifyCard(card); !ok {
			t.Fatalf("restored card %s invalid: %s", card.Agent, why)
		}
	}
	if !found["provider-a"] || !found["provider-b"] {
		t.Fatalf("missing restored agents: %v", found)
	}
}
