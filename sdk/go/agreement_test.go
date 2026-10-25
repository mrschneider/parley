package parley

import (
	"strings"
	"testing"
	"time"
)

func settlementMessages(t *testing.T) (*Message, *Message, *Message) {
	t.Helper()
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

	request := NewMessage("planner-42", "dispatcher-7", now)
	request.Tenant = "acme"
	request.Act = "request"
	request.Target = "route.plan"
	request.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	request.Expect = []Clause{
		{ID: "c1", Check: &Check{Field: "route.eta_min", Op: "lte", Value: 150}, Weight: 1},
		{ID: "c2", NL: "Evitar obras nas próximas 24h", Weight: 0.9},
	}
	AssignID(request)
	request = Sign(request, "planner-42", "acme")

	offer := NewMessage("dispatcher-7", "planner-42", now.Add(time.Minute))
	offer.Tenant = "acme"
	offer.Act = "propose"
	offer.Target = "route.plan"
	offer.Parent = request.ID
	offer.Body = map[string]any{"route": map[string]any{"eta_min": 100}}
	offer.Clauses = []string{"c2"}
	AssignID(offer)
	offer = Sign(offer, "dispatcher-7", "acme")

	acceptance := NewMessage("planner-42", "dispatcher-7", now.Add(2*time.Minute))
	acceptance.Tenant = "acme"
	acceptance.Act = "commit"
	acceptance.Target = "route.plan"
	acceptance.Parent = offer.ID
	acceptance.Clauses = []string{"c2"}
	AssignID(acceptance)
	acceptance = Sign(acceptance, "planner-42", "acme")

	return request, offer, acceptance
}

func TestBuildAgreementMaterializesBindingClausesAndBody(t *testing.T) {
	request, offer, acceptance := settlementMessages(t)
	agreement, err := BuildAgreement(request, offer, acceptance)
	if err != nil {
		t.Fatal(err)
	}
	if agreement.Target != "route.plan" || agreement.Act != "commit" {
		t.Fatalf("unexpected agreement target/act: %s/%s", agreement.Target, agreement.Act)
	}
	if agreement.Requestor.ID != "planner-42" || agreement.Provider.ID != "dispatcher-7" {
		t.Fatalf("unexpected participants: %+v %+v", agreement.Requestor, agreement.Provider)
	}
	if strings.Join(agreement.BindingClauses, ",") != "c2" {
		t.Fatalf("binding clauses = %v", agreement.BindingClauses)
	}
	if len(agreement.Body) != 1 {
		t.Fatalf("agreement body = %#v", agreement.Body)
	}
	if agreement.ID != "" || len(agreement.Signatures) != 0 {
		t.Fatal("built agreement should be unsigned until explicitly signed")
	}
}

func TestAgreementContentIDAndDualSignature(t *testing.T) {
	request, offer, acceptance := settlementMessages(t)
	agreement, err := BuildAgreement(request, offer, acceptance)
	if err != nil {
		t.Fatal(err)
	}

	ks := NewKeyStore()
	for _, agent := range []string{"planner-42", "dispatcher-7"} {
		if _, err := ks.Generate(agent, "acme"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := SignAgreement(agreement, "planner-42", "acme", ks.MustGet("planner-42", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := VerifyAgreement(first); !ok {
		t.Fatalf("single-signature agreement invalid: %s", why)
	}

	// Any change to the receipt must invalidate the content id.
	tampered := *first
	tampered.BindingClauses = []string{"c1", "c2"}
	if ok, why := VerifyAgreement(&tampered); ok {
		t.Fatal("tampered agreement verified")
	} else if why != "id de conteúdo divergente" {
		t.Fatalf("unexpected tamper reason: %s", why)
	}

	both, err := SignAgreement(first, "dispatcher-7", "acme", ks.MustGet("dispatcher-7", "acme"))
	if err != nil {
		t.Fatal(err)
	}
	if len(both.Signatures) != 2 {
		t.Fatalf("signatures = %d, want 2", len(both.Signatures))
	}
	if ok, why := VerifyAgreement(both); !ok {
		t.Fatalf("dual-signed agreement invalid: %s", why)
	}
}

func TestBuildAgreementRejectsMismatchedThread(t *testing.T) {
	request, offer, acceptance := settlementMessages(t)
	acceptance.Parent = "msg:0000000000000000"
	if _, err := BuildAgreement(request, offer, acceptance); err == nil {
		t.Fatal("expected mismatched thread to fail")
	}
}
