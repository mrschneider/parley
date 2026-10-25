// Package transport — Supervisor: enforcement runtime for deadline + fallback.
//
// The schema declares "deadline" and "fallback" ("routed here if the
// primary declines or misses the deadline"), but a declaration is not
// enforcement: without a watcher, nothing ever routes to the fallback.
//
// Supervisor is that watcher. It implements the escrow model:
//
//  1. Track: a message with a deadline (and optionally a fallback) is kept
//     in escrow by its sender.
//  2. Watch: the supervisor checks tracked messages against the clock.
//  3. Escalate: on deadline miss, the ORIGINAL envelope is re-delivered to
//     the fallback's inbox (transport-agnostic sink). On explicit decline
//     (reject) by the primary, escalation is immediate.
//
// Safety:
//   - a message is escalated at most ONCE (deadline miss + late reject, or
//     two ticks, never double-route)
//   - a reply from the primary cancels the escalation (and a late reject
//     after a reply is a no-op)
//   - the supervisor never fabricates message content: the escalation
//     payload is the original signed envelope; application code re-targets
//     (to/provenance) and re-signs when it forwards (see DeliverFallback)
//
// Driving: Tick() processes due entries against the injected clock —
// deterministic, no sleeps, used by tests. Run() loops Tick() on a timer
// for production.
package transport

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	parley "example.org/parley-go"
)

// FallbackDelivery is the receipt of one escalation.
type FallbackDelivery struct {
	ID        string    `json:"id"`                   // original tracked message id
	To        string    `json:"to"`                   // fallback agent id
	Reason    string    `json:"reason"`               // "deadline" | "decline"
	At        time.Time `json:"at"`                   // when the escalation fired
	Original  []byte    `json:"-"`                    // raw envelope (as received)
	Delivered []byte    `json:"-"`                    // raw envelope as handed to the sink
	ReplyID   string    `json:"reply_id,omitempty"`   // primary reply (decline audit)
}

// FallbackSink receives escalated messages. In production this is a queue/
// HTTP/WS adapter that pushes the envelope to the fallback's inbox; in
// tests it is an in-memory recorder.
type FallbackSink func(to string, raw []byte) *Result

// Supervisor watches deadlines and routes fallbacks.
type Supervisor struct {
	mu        sync.Mutex
	tracked   map[string]*trackEntry
	done      map[string]bool       // original id -> already settled (reply/escalation)
	sinks     map[string]FallbackSink
	delivered []FallbackDelivery    // audit trail
	now       func() time.Time
	wake      chan struct{}
	stop      bool
}

type trackEntry struct {
	raw      []byte
	msg      *parley.Message
	deadline time.Time
	fbTo     string
}

// NewSupervisor builds a supervisor. Sinks are registered per fallback
// agent id (the agent the escalation is routed to).
func NewSupervisor() *Supervisor {
	return &Supervisor{
		tracked: map[string]*trackEntry{},
		done:    map[string]bool{},
		sinks:   map[string]FallbackSink{},
		now:     time.Now,
		wake:    make(chan struct{}, 1),
	}
}

// WithClock injects the clock (tests drive time deterministically).
func (s *Supervisor) WithClock(f func() time.Time) *Supervisor {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = f
	return s
}

// RegisterSink binds a fallback agent id to a delivery sink.
func (s *Supervisor) RegisterSink(agent string, sink FallbackSink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks[agent] = sink
}

// Track puts a message in escrow: it is escalated to its fallback if the
// deadline passes without a reply from the primary. Returns the deadline.
func (s *Supervisor) Track(raw []byte) (time.Time, error) {
	var m parley.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return time.Time{}, fmt.Errorf("supervisor: json inválido: %w", err)
	}
	if m.ID == "" {
		return time.Time{}, fmt.Errorf("supervisor: mensagem sem id")
	}
	if m.Deadline == "" {
		return time.Time{}, fmt.Errorf("supervisor: mensagem sem deadline não é rastreável")
	}
	dl, err := time.Parse("2006-01-02T15:04:05Z", m.Deadline)
	if err != nil {
		return time.Time{}, fmt.Errorf("supervisor: deadline malformado: %s", m.Deadline)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tracked[m.ID]; !ok {
		s.tracked[m.ID] = &trackEntry{
			raw:      append([]byte(nil), raw...),
			msg:      &m,
			deadline: dl,
			fbTo:     fbAgent(&m),
		}
	}
	s.signal()
	return dl, nil
}

// Reply marks that the primary answered msgID (replyID for the audit
// trail). Cancels any pending escalation; a late reject after a reply is
// a no-op.
func (s *Supervisor) Reply(msgID, replyID string) {
	s.mu.Lock()
	delete(s.tracked, msgID)
	s.done[msgID] = true
	s.signal()
	s.mu.Unlock()
}

// Decline marks that the primary explicitly rejected msgID (reject act):
// "routed here if the primary declines". Triggers immediate escalation.
func (s *Supervisor) Decline(msgID, replyID string) {
	s.mu.Lock()
	e, ok := s.tracked[msgID]
	if ok {
		delete(s.tracked, msgID)
	}
	s.mu.Unlock()
	if ok {
		s.escalate(e, "decline", replyID)
	}
}

// Wire inspects a delivered raw message and updates escrow state
// accordingly — the single hook application runtimes call after each
// Inbox.Deliver:
//   - a message with deadline (and a fallback) enters escrow
//   - a reply (parent set) to a tracked message cancels its escalation
//   - a reject to a tracked message escalates immediately
func (s *Supervisor) Wire(raw []byte) {
	var m parley.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	if m.Deadline != "" && m.Fallback != nil && m.Fallback.To.ID != "" {
		if _, err := s.Track(raw); err == nil {
			return
		}
	}
	if m.Parent != "" {
		switch m.Act {
		case "reject", "cancel":
			s.Decline(m.Parent, m.ID)
		default:
			s.Reply(m.Parent, m.ID)
		}
	}
}

// Tick processes every tracked message whose deadline has passed (against
// the injected clock), escalating each to its fallback. Returns the
// deliveries that fired (deterministic order by original id).
func (s *Supervisor) Tick() []FallbackDelivery {
	s.mu.Lock()
	now := s.now()
	var due []*trackEntry
	for id, e := range s.tracked {
		if !e.deadline.After(now) {
			due = append(due, e)
			delete(s.tracked, id)
		}
	}
	s.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].msg.ID < due[j].msg.ID })
	var out []FallbackDelivery
	for _, e := range due {
		if d, ok := s.escalate(e, "deadline", ""); ok {
			out = append(out, d)
		}
	}
	return out
}

// Run loops Tick() on a timer until Stop (production path).
func (s *Supervisor) Run() {
	for {
		s.mu.Lock()
		stopped := s.stop
		var next time.Time
		for _, e := range s.tracked {
			if next.IsZero() || e.deadline.Before(next) {
				next = e.deadline
			}
		}
		s.mu.Unlock()
		if stopped {
			return
		}
		if next.IsZero() {
			<-s.wake
			continue
		}
		d := time.Until(next)
		if d > 0 {
			select {
			case <-time.After(d):
			case <-s.wake:
			}
		}
		s.Tick()
	}
}

// Stop ends the Run loop.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	s.stop = true
	s.signal()
	s.mu.Unlock()
}

// Pending returns the ids of messages still in escrow (deterministic).
func (s *Supervisor) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.tracked))
	for id := range s.tracked {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SettleLog returns the audit trail of escalations (deterministic).
func (s *Supervisor) SettleLog() []FallbackDelivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FallbackDelivery, len(s.delivered))
	copy(out, s.delivered)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// escalate performs at-most-once delivery of the fallback.
func (s *Supervisor) escalate(e *trackEntry, reason, replyID string) (FallbackDelivery, bool) {
	s.mu.Lock()
	if e.fbTo == "" || s.done[e.msg.ID] {
		s.mu.Unlock()
		return FallbackDelivery{}, false
	}
	s.done[e.msg.ID] = true
	sink, ok := s.sinks[e.fbTo]
	s.mu.Unlock()
	if !ok {
		return FallbackDelivery{}, false
	}
	raw := e.raw
	res := sink(e.fbTo, raw)
	d := FallbackDelivery{
		ID: e.msg.ID, To: e.fbTo, Reason: reason,
		At:        s.now(),
		Original:  raw,
		Delivered: append([]byte(nil), raw...),
		ReplyID:   replyID,
	}
	_ = res
	s.mu.Lock()
	s.delivered = append(s.delivered, d)
	s.mu.Unlock()
	return d, true
}

// DeliverFallback builds the escalation envelope: the original message
// re-targeted at the fallback agent (caller sets "to"), with provenance
// extended by the original id (chain of trust), the consumed fallback
// field removed, and the content id INVALIDATED (the envelope changed —
// the caller MUST AssignID + re-sign before forwarding; the old id would
// be a content-address mismatch).
func DeliverFallback(original *parley.Message) *parley.Message {
	out := *original
	out.Fallback = nil
	out.ID = ""
	if !contains(out.Provenance, original.ID) {
		out.Provenance = append(append([]string(nil), out.Provenance...), original.ID)
	}
	return &out
}

func (s *Supervisor) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func contains(ss []string, v string) bool {
	for _, x := range ss {
		if x == v {
			return true
		}
	}
	return false
}

func fbAgent(m *parley.Message) string {
	if m.Fallback != nil {
		return m.Fallback.To.ID
	}
	return ""
}
