package transport

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	parley "example.org/parley-go"
)

var runtimeT0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// publishCard signs and publishes a card for agent. The public key comes
// from the production KeyStore, so the card is the trust anchor.
func publishCard(t *testing.T, registry *parley.Registry, ks *parley.KeyStore, agent, tenant string) {
	t.Helper()
	pub, ok := ks.PubKey(agent, tenant)
	if !ok {
		t.Fatalf("missing key for %s", agent)
	}
	card := &parley.Card{
		V:         parley.Version,
		Agent:     agent,
		Tenant:    tenant,
		Endpoint:  "runtime://" + agent,
		Transport: "queue",
		Pub: parley.CardPubKey{
			Alg: "ed25519",
			Key: base64.StdEncoding.EncodeToString(pub),
		},
		Provides: []parley.CardCapability{{Name: "route.plan", Version: 1}},
	}
	signed := parley.SignCardWith(card, ks.MustGet(agent, tenant))
	if err := registry.Publish(signed, agent, runtimeT0); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeConnectsRegistryInboxAndSupervisor(t *testing.T) {
	tenant := "acme"
	agents := []string{"planner-42", "dispatcher-7", "dispatcher-backup"}
	ks, _, err := parley.LoadOrGenerate(t.TempDir()+"/keys.json", tenant, agents)
	if err != nil {
		t.Fatal(err)
	}
	registry := parley.NewRegistry()
	for _, agent := range agents {
		publishCard(t, registry, ks, agent, tenant)
	}

	primary := NewInbox(WithTenantFilter(tenant), WithCardRegistry(registry))
	fallback := NewInbox(WithTenantFilter(tenant), WithCardRegistry(registry))
	inboxes := map[string]*Inbox{
		"dispatcher-7":      primary,
		"dispatcher-backup": fallback,
	}
	sup := NewSupervisor().WithClock(func() time.Time { return runtimeT0 })
	sup.RegisterSink("dispatcher-backup", CompleteFallbackSink(ks, tenant, inboxes))

	// A production sender: deadline + fallback, signed with a real key.
	request := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup", "2026-10-23T09:05:00Z")
	request, err = ks.SignWith(request, tenant)
	if err != nil {
		t.Fatal(err)
	}
	requestRaw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	// The primary receives it through the registry trust path. It does not
	// reply; the supervisor enforces the declared deadline.
	if res := primary.Deliver(requestRaw); res.Status != StatusAccepted {
		t.Fatalf("primary rejected request: %s %s", res.Status, res.Reason)
	}
	if _, err = sup.Track(requestRaw); err != nil {
		t.Fatal(err)
	}
	sup.WithClock(func() time.Time { return runtimeT0.Add(10 * time.Minute) })
	deliveries := sup.Tick()
	if len(deliveries) != 1 || deliveries[0].Reason != "deadline" {
		t.Fatalf("expected deadline escalation, got %+v", deliveries)
	}
	if fallback.Len() != 1 {
		t.Fatalf("fallback inbox len=%d, want 1", fallback.Len())
	}

	// Resolve the escalated envelope (fresh content id) and audit it.
	var escalated *parley.Message
	fallback.mu.Lock()
	for _, m := range fallback.store {
		escalated = m
	}
	fallback.mu.Unlock()
	if escalated == nil {
		t.Fatal("escalated message missing")
	}
	if escalated.To.ID != "dispatcher-backup" {
		t.Fatalf("escalated to %s", escalated.To.ID)
	}
	found := false
	for _, id := range escalated.Provenance {
		if id == request.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("provenance %v missing original id", escalated.Provenance)
	}
	// The registered planner card verifies the re-signed escalation.
	if ok, why := registry.VerifyMessageWithCard(escalated); !ok {
		t.Fatalf("escalation not valid under registry trust: %s", why)
	}
}
