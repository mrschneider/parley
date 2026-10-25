package transport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	parley "example.org/parley-go"
)

var t0 = time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)

// buildTracked builds a signed message with deadline + fallback (the shape
// a sender puts in escrow).
func buildTracked(t *testing.T, from, to, fb string, deadline string) *parley.Message {
	t.Helper()
	m := parley.NewMessage(from, to, t0)
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	m.Deadline = deadline
	m.Fallback = &parley.Fallback{To: parley.AgentRef{ID: fb}, Reason: "timeout"}
	parley.AssignID(m)
	return parley.Sign(m, from, "acme")
}

func buildReply(t *testing.T, from, to, act, parentID string) *parley.Message {
	t.Helper()
	m := parley.NewMessage(from, to, t0.Add(30*time.Second))
	m.Tenant = "acme"
	m.Act = act
	m.Parent = parentID
	m.Body = map[string]any{"route": map[string]any{"eta_min": 142}}
	parley.AssignID(m)
	return parley.Sign(m, from, "acme")
}

func raw(t *testing.T, m *parley.Message) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSupervisorDeadlineEscalation(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	fbInbox := NewInbox()
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		return fbInbox.Deliver(b)
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	rawB := raw(t, req)
	dl, err := sup.Track(rawB)
	if err != nil {
		t.Fatal(err)
	}
	if !dl.Equal(time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC)) {
		t.Fatalf("deadline = %v", dl)
	}
	if len(sup.Pending()) != 1 {
		t.Fatalf("Pending = %v, expected 1", sup.Pending())
	}

	// before the deadline: nothing
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("escalated before the deadline: %v", d)
	}
	// no deadline: escalation to the fallback
	now := t0.Add(5 * time.Minute)
	sup.WithClock(func() time.Time { return now })
	del := sup.Tick()
	if len(del) != 1 {
		t.Fatalf("expected 1 escalation at the deadline, got %v", del)
	}
	if del[0].Reason != "deadline" || del[0].To != "dispatcher-backup" {
		t.Fatalf("wrong escalation: %+v", del[0])
	}
	if len(sup.Pending()) != 0 {
		t.Fatalf("escrow not released after escalation: %v", sup.Pending())
	}
	// the fallback inbox received the original envelope (signed by the sender)
	if fbInbox.Len() != 1 {
		t.Fatalf("fallback inbox = %d, expected 1 (original envelope delivered)", fbInbox.Len())
	}
	// idempotency: ticking again does not re-escalate
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("re-escalated after deadline: %v", d)
	}
}

func TestSupervisorDeclineEscalation(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	var got []string
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		got = append(got, to)
		return &Result{Status: StatusAccepted}
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:30:00Z")
	if _, err := sup.Track(raw(t, req)); err != nil {
		t.Fatal(err)
	}
	// primary explicitly rejects, LONG before the deadline
	rej := buildReply(t, "dispatcher-7", "planner-42", "reject", req.ID)
	sup.Decline(req.ID, rej.ID)
	if len(got) != 1 || got[0] != "dispatcher-backup" {
		t.Fatalf("decline did not escalate immediately: %v", got)
	}
	log := sup.SettleLog()
	if len(log) != 1 || log[0].Reason != "decline" || log[0].ReplyID != rej.ID {
		t.Fatalf("wrong escalation log: %+v", log)
	}
	// deadline passes later: nothing (already settled)
	sup.WithClock(func() time.Time { return t0.Add(31 * time.Minute) })
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("re-escalated after decline: %v", d)
	}
}

func TestSupervisorReplyCancels(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	calls := 0
	sup.RegisterSink("dispatcher-backup", func(string, []byte) *Result {
		calls++
		return &Result{Status: StatusAccepted}
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	if _, err := sup.Track(raw(t, req)); err != nil {
		t.Fatal(err)
	}
	// primary replies (propose) before the deadline -> cancels the escalation
	prop := buildReply(t, "dispatcher-7", "planner-42", "propose", req.ID)
	sup.Reply(req.ID, prop.ID)
	// deadline passes: no escalation
	sup.WithClock(func() time.Time { return t0.Add(6 * time.Minute) })
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("escalated despite the reply: %v", d)
	}
	if calls != 0 {
		t.Fatalf("fallback called %d times after reply", calls)
	}
	// late reject after reply: no-op
	sup.Decline(req.ID, "msg:late")
	if calls != 0 {
		t.Fatal("late reject after reply should not escalate")
	}
}

func TestSupervisorWire(t *testing.T) {
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	var escalated []string
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		escalated = append(escalated, to)
		return &Result{Status: StatusAccepted}
	})

	// Wire: a message with deadline+fallback enters escrow
	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	sup.Wire(raw(t, req))
	if len(sup.Pending()) != 1 {
		t.Fatalf("Wire did not put it in escrow: %v", sup.Pending())
	}

	// Wire: reply do primary cancela
	prop := buildReply(t, "dispatcher-7", "planner-42", "propose", req.ID)
	sup.Wire(raw(t, prop))
	if len(sup.Pending()) != 0 {
		t.Fatalf("Wire of the reply did not cancel escrow: %v", sup.Pending())
	}

	// Wire: reject do primary escala
	req2 := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:40:00Z")
	sup.Wire(raw(t, req2))
	rej := buildReply(t, "dispatcher-7", "planner-42", "reject", req2.ID)
	sup.Wire(raw(t, rej))
	if len(escalated) != 1 {
		t.Fatalf("Wire of the reject did not escalate: %v", escalated)
	}
}

func TestSupervisorNoFallbackNoDeadline(t *testing.T) {
	sup := NewSupervisor()
	// no deadline: not trackable
	m := buildTracked(t, "a", "b", "c", "")
	m.Deadline = ""
	parley.AssignID(m)
	m = parley.Sign(m, "a", "acme")
	if _, err := sup.Track(raw(t, m)); err == nil {
		t.Fatal("message without deadline should reject Track")
	}
	// no fallback: trackable, but escalation is a no-op
	m2 := buildTracked(t, "a", "b", "c", "2026-10-23T09:01:00Z")
	m2.Fallback = nil
	parley.AssignID(m2)
	m2 = parley.Sign(m2, "a", "acme")
	if _, err := sup.Track(raw(t, m2)); err != nil {
		t.Fatal(err)
	}
	sup.WithClock(func() time.Time { return t0.Add(2 * time.Minute) })
	if d := sup.Tick(); len(d) != 0 {
		t.Fatalf("without fallback there is no escalation: %v", d)
	}
}

func TestDeliverFallbackRetarget(t *testing.T) {
	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	out := DeliverFallback(req)
	// the original envelope is immutable
	if req.Fallback == nil || req.To.ID != "dispatcher-7" {
		t.Fatal("DeliverFallback mutou o original")
	}
	// fallback consumido, provenance estendida
	if out.Fallback != nil {
		t.Fatal("fallback field should have been consumed in the escalation envelope")
	}
	if len(out.Provenance) != 1 || out.Provenance[0] != req.ID {
		t.Fatalf("wrong escalation provenance: %v", out.Provenance)
	}
	// content id invalidated (envelope changed) — the caller runs AssignID
	// before re-signing; the old id would be a content mismatch
	if out.ID != "" {
		t.Fatal("escalation should invalidate the content id (new envelope)")
	}
}

func TestSupervisorEndToEndThroughInbox(t *testing.T) {
	// Full scenario: the sender makes a request with deadline+fallback; the
	// primary never replies; the supervisor escalates and the envelope
	// (re-signed by the sender, re-targeted) arrives at the fallback inbox
	// as a NEW message (accepted, not a replay).
	sup := NewSupervisor().WithClock(func() time.Time { return t0 })
	fbInbox := NewInbox()
	sup.RegisterSink("dispatcher-backup", func(to string, b []byte) *Result {
		return fbInbox.Deliver(b)
	})

	req := buildTracked(t, "planner-42", "dispatcher-7", "dispatcher-backup",
		"2026-10-23T09:05:00Z")
	if _, err := sup.Track(raw(t, req)); err != nil {
		t.Fatal(err)
	}
	sup.WithClock(func() time.Time { return t0.Add(5 * time.Minute) })
	del := sup.Tick()
	if len(del) != 1 {
		t.Fatalf("escalation did not fire: %v", del)
	}
	// the escalation delivers the ORIGINAL envelope (signed by planner-42):
	// signature verification passes at the fallback inbox
	res := del[0]
	if !strings.Contains(string(res.Delivered), "dispatcher-backup") {
		t.Fatal("escalated envelope should be the original (with fallback field)")
	}
	if fbInbox.Len() != 1 {
		t.Fatalf("fallback inbox should have 1 message, has %d", fbInbox.Len())
	}
	// and the sender's re-target + re-sign produce a new, verifiable message
	esc := DeliverFallback(req)
	esc.To = parley.AgentRef{ID: "dispatcher-backup"}
	parley.AssignID(esc) // new envelope -> new content id
	esc = parley.Sign(esc, "planner-42", "acme")
	r := fbInbox.Deliver(raw(t, esc))
	if r.Status != StatusAccepted {
		t.Fatalf("re-target+re-sign escalation should be accepted: %s (%s)", r.Status, r.Reason)
	}
	if fbInbox.Len() != 2 {
		t.Fatalf("fallback inbox = %d, expected 2", fbInbox.Len())
	}
}
