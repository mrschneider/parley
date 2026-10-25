package transport

import (
	"testing"
	"time"

	parley "example.org/parley-go"
)

func TestCompleteFallbackSinkDeliversRetargetedSignedMessage(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	tenant := "acme"
	agents := []string{"planner-42", "dispatcher-7", "dispatcher-backup"}

	ks, pubs, err := parley.LoadOrGenerate(t.TempDir()+"/keys.json", tenant, agents)
	if err != nil {
		t.Fatal(err)
	}

	request := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup", "2026-10-23T09:05:00Z")
	request, err = ks.SignWith(request, tenant)
	if err != nil {
		t.Fatal(err)
	}
	requestRaw := raw(t, request)

	primaryInbox := NewInbox()
	fallbackInbox := NewInbox()
	inboxes := map[string]*Inbox{
		"dispatcher-7":      primaryInbox,
		"dispatcher-backup": fallbackInbox,
	}
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	sup.RegisterSink("dispatcher-backup", CompleteFallbackSink(ks, tenant, inboxes))

	// Verify the sender's own signature first using the card's public key.
	if ok, why := parley.VerifyWithKey(request, pubs["planner-42"]); !ok {
		t.Fatalf("sender signature invalid: %s", why)
	}

	_, err = sup.Track(requestRaw)
	if err != nil {
		t.Fatal(err)
	}
	sup.WithClock(func() time.Time { return t0.Add(10 * time.Minute) })
	deliveries := sup.Tick()
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 fallback delivery, got %d", len(deliveries))
	}
	if fallbackInbox.Len() != 1 {
		t.Fatalf("fallback inbox length = %d, want 1", fallbackInbox.Len())
	}

	// The escalated envelope has a fresh content id, so retrieve it from the
	// fallback inbox directly rather than by the original request id.
	delivered, ok := fallbackInbox.Get(deliveries[0].ID)
	if !ok {
		var deliveredID string
		fallbackInbox.mu.Lock()
		for id := range fallbackInbox.store {
			deliveredID = id
		}
		fallbackInbox.mu.Unlock()
		delivered, ok = fallbackInbox.Get(deliveredID)
	}
	if !ok {
		t.Fatalf("delivered message %s not stored", deliveries[0].ID)
	}
	if delivered.To.ID != "dispatcher-backup" {
		t.Fatalf("delivered to %s, want dispatcher-backup", delivered.To.ID)
	}
	if delivered.Fallback != nil {
		t.Fatal("fallback field should have been consumed")
	}
	foundProvenance := false
	for _, id := range delivered.Provenance {
		if id == request.ID {
			foundProvenance = true
		}
	}
	if !foundProvenance {
		t.Fatalf("provenance %v does not include original id %s", delivered.Provenance, request.ID)
	}
	if delivered.ID == request.ID {
		t.Fatal("escalated message reused the original content id")
	}

	cardPub := pubs["planner-42"]
	if ok, why := parley.VerifyWithKey(delivered, cardPub); !ok {
		t.Fatalf("escalated message signature invalid: %s", why)
	}
}
