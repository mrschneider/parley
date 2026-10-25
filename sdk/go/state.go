// PARLEY v0 — Act state machine (negotiation thread).
//
// The schema's 11 acts are the events of a conversation; this file gives
// the conversation a state: a negotiation Thread is opened by a request
// (with target) and walks the acts under a minimal grammar, with an
// explicit terminal (accept / commit / reject / cancel) and negotiation
// loops (propose/counter) with a configurable cap.
//
// Semantics (mirrors the Python reference, byte for byte in decisions):
//
//   - open:  only a request with target (a negotiation's contract is born
//     from a request; inform/ask do not open a thread)
//   - offer: propose or counter; counter requires a previous offer
//     (you do not "counter-offer" the empty)
//   - final: accept / commit / reject / cancel -> terminal state; any
//     subsequent act is invalid (thread closed)
//   - reject/cancel also end the negotiation (there is no "I reject, and
//     then...?")
//   - delegate: the agent forwards the subject; the thread continues (the
//     delegate replies), but the act is recorded as a delegation
//   - inform/ask/verify: lateral acts — they inform without changing the
//     state (the thread is not about them)
//   - loop cap: N consecutive proposals/counters without an end -> the
//     next offer is invalid (negotiations do not terminate; the cap is a
//     pragmatic, configurable ceiling)
//
// The Thread does not fabricate trust: it only organizes the grammar.
// Signature, content id and the hybrid contract remain what provide the
// guarantees.
package parley

import "fmt"

// State of the negotiation thread.
type ThreadState string

const (
	StateNew         ThreadState = "new"         // not opened yet
	StateNegotiating ThreadState = "negotiating" // in progress
	StateSettled     ThreadState = "settled"     // accept/commit (agreement)
	StateRejected    ThreadState = "rejected"    // reject/cancel (no agreement)
)

// Terminal states: after one of these, no further act is valid.
func (s ThreadState) Terminal() bool {
	return s == StateSettled || s == StateRejected
}

// Thread is one negotiation: a request (with target) plus the acts that
// follow it. The zero value is a closed (New) thread.
type Thread struct {
	ID         string      // root message id (the request)
	Target     string      // capability under negotiation
	State      ThreadState // current state
	LastOffer  string      // id of the latest propose/counter
	OfferCount int         // offers seen since open (loop cap)
	Cap        int         // max consecutive offers before the thread stalls
	Trace      []TraceStep // ordered log of applied acts (audit)
}

// TraceStep is one applied act (deterministic audit trail).
type TraceStep struct {
	Act   string `json:"act"`
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	State string `json:"state"` // state AFTER the act
}

// NewThread builds an unopened thread with the given loop cap (<=0 -> 8).
func NewThread(cap int) *Thread {
	if cap <= 0 {
		cap = 8
	}
	return &Thread{State: StateNew, Cap: cap}
}

// Apply transitions the thread with one message. It returns an error
// describing the first grammar violation (the message is NOT applied).
func (t *Thread) Apply(m *Message) error {
	if m == nil {
		return fmt.Errorf("thread: nil message")
	}
	switch t.State {
	case StateNew:
		return t.open(m)
	case StateNegotiating:
		return t.step(m)
	default: // settled / rejected
		return fmt.Errorf("thread %s: closed at %s — act '%s' (%s) invalid",
			t.ID, t.State, m.Act, m.ID)
	}
}

func (t *Thread) open(m *Message) error {
	if m.Act != "request" || m.Target == "" {
		return fmt.Errorf("thread: open requires 'request' with target (got '%s' %s)", m.Act, m.ID)
	}
	t.ID = m.ID
	t.Target = m.Target
	t.State = StateNegotiating
	t.Trace = append(t.Trace, TraceStep{m.Act, m.ID, m.From.ID, m.To.ID, string(t.State)})
	return nil
}

func (t *Thread) step(m *Message) error {
	switch m.Act {
	case "propose":
		if err := t.offer(m); err != nil {
			return err
		}
	case "counter":
		if t.LastOffer == "" {
			return fmt.Errorf("thread: 'counter' requires a previous offer (thread %s, msg %s)", t.ID, m.ID)
		}
		if err := t.offer(m); err != nil {
			return err
		}
	case "accept", "commit":
		t.State = StateSettled
		t.trace(m)
	case "reject", "cancel":
		t.State = StateRejected
		t.trace(m)
	case "delegate":
		// the subject continues with the delegate; the thread does not change state
		t.trace(m)
	case "inform", "ask", "verify":
		// lateral acts: recorded, no state change
		t.trace(m)
	default:
		return fmt.Errorf("thread: act '%s' not recognized (msg %s)", m.Act, m.ID)
	}
	return nil
}

func (t *Thread) offer(m *Message) error {
	if t.OfferCount+1 > t.Cap {
		return fmt.Errorf("thread: offer cap exceeded (%d) — negotiation without end (thread %s, msg %s)",
			t.Cap, t.ID, m.ID)
	}
	t.OfferCount++
	t.LastOffer = m.ID
	t.trace(m)
	return nil
}

func (t *Thread) trace(m *Message) {
	t.Trace = append(t.Trace, TraceStep{m.Act, m.ID, m.From.ID, m.To.ID, string(t.State)})
}

// NewThreadFrom builds a thread from the ordered messages of one
// negotiation (root first). Returns the thread and any first violation.
func NewThreadFrom(cap int, msgs []*Message) (*Thread, error) {
	th := NewThread(cap)
	for _, m := range msgs {
		if err := th.Apply(m); err != nil {
			return th, err
		}
	}
	return th, nil
}
