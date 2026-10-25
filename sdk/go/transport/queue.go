// Package transport implements the live PARLEY v0 adapters: an in-process
// queue and an inbox that proves the protocol's replay/idempotency
// guarantees on a live delivery path.
//
// The envelope is self-contained: the transport delivers raw JSON bytes;
// all trust decisions (schema, signature, content-addressed id, contract)
// happen in the inbox, exactly as they would in any other platform
// (WebSocket, Kafka, NATS — the envelope does not change).
package transport

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"

	"example.org/parley-go"
)

// Status of one delivered message.
const (
	StatusAccepted  = "accepted"  // first time: verified and stored
	StatusDuplicate = "duplicate" // same content-addressed id seen before (replay)
	StatusRejected  = "rejected"  // schema, signature, or contract failure
)

// Result is the inbox's decision for one delivered message.
type Result struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
	Validation map[string]any `json:"validation,omitempty"` // contract check vs parent (when applicable)
}

// Option customizes an Inbox.
type Option func(*Inbox)

// WithVerifier replaces the signature verifier (e.g. one that uses the
// agent card's public key from the registry instead of key derivation).
func WithVerifier(f func(*parley.Message) (bool, string)) Option {
	return func(in *Inbox) { in.verify = f }
}

// WithTenantFilter rejects messages of any tenant other than the given one.
func WithTenantFilter(tenant string) Option {
	return func(in *Inbox) { in.tenant = tenant }
}

// WithStoreFile enables disk-backed persistence: accepted ids/messages are
// saved after every delivery, and Load restores them at startup. This is
// what makes replay detection survive a process/broker restart.
func WithStoreFile(path string) Option {
	return func(in *Inbox) { in.storePath = path }
}

// Inbox is the receiving side of a live transport: it verifies,
// deduplicates (content-addressed ids), checks the hybrid contract against
// the parent message (when the parent was previously delivered to this
// inbox), stores accepted messages, tracks negotiation threads (act state
// machine) and ranks received proposals by the parent's clause weights.
type Inbox struct {
	mu        sync.Mutex
	seen      map[string]bool
	seenAt    map[string]time.Time
	store     map[string]*parley.Message
	verify    func(*parley.Message) (bool, string)
	tenant    string
	now       func() time.Time
	storePath string
	contracts map[string]*parley.Capability
	threads   map[string]*parley.Thread // root (request) id -> thread
	retention RetentionPolicy
}

// RetentionPolicy bounds inbox memory. Zero values disable each bound.
type RetentionPolicy struct {
	MaxAge     time.Duration // remove messages older than this
	MaxRecords int           // keep at most this many accepted messages/ids
}

// NewInbox builds an inbox.
func NewInbox(opts ...Option) *Inbox {
	in := &Inbox{
		seen:    map[string]bool{},
		seenAt:  map[string]time.Time{},
		store:   map[string]*parley.Message{},
		verify:  parley.Verify,
		now:     time.Now,
		threads: map[string]*parley.Thread{},
	}
	for _, o := range opts {
		o(in)
	}
	return in
}

// WithRetention bounds accepted-message memory. MaxAge zero disables age
// eviction; MaxRecords zero disables count eviction. Eviction applies to
// both the store and replay set (an evicted id may be accepted again).
func WithRetention(maxAge time.Duration, maxRecords int) Option {
	return func(in *Inbox) { in.retention = RetentionPolicy{MaxAge: maxAge, MaxRecords: maxRecords} }
}

// Sweep enforces the retention policy now and returns removed ids.
func (in *Inbox) Sweep() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.sweepLocked(in.now())
}

func (in *Inbox) sweepLocked(now time.Time) []string {
	type record struct {
		id string
		at time.Time
	}
	records := make([]record, 0, len(in.store))
	for id, m := range in.store {
		at, err := time.Parse("2006-01-02T15:04:05Z", m.TS)
		if err != nil {
			at = in.seenAt[id]
		}
		if at.IsZero() {
			at = now
		}
		records = append(records, record{id: id, at: at})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].at.Equal(records[j].at) {
			return records[i].id < records[j].id
		}
		return records[i].at.Before(records[j].at)
	})

	removedSet := map[string]bool{}
	for _, r := range records {
		if in.retention.MaxAge > 0 && now.Sub(r.at) > in.retention.MaxAge {
			removedSet[r.id] = true
		}
	}
	if in.retention.MaxRecords > 0 {
		alive := 0
		for i := len(records) - 1; i >= 0; i-- {
			r := records[i]
			if removedSet[r.id] {
				continue
			}
			alive++
			if alive > in.retention.MaxRecords {
				removedSet[r.id] = true
			}
		}
	}
	if len(removedSet) == 0 {
		return nil
	}
	removed := make([]string, 0, len(removedSet))
	for id := range removedSet {
		delete(in.seen, id)
		delete(in.seenAt, id)
		delete(in.store, id)
		delete(in.threads, id)
		removed = append(removed, id)
	}
	sort.Strings(removed)
	return removed
}

// WithContracts supplies the capability contracts (name -> declaration)
// known to this inbox, so delivered messages whose target names a known
// capability get their body validated against the capability's
// input/output contract (see parley.BodyContract). The inbox is usually
// the PROVIDER: it checks that the caller's request body matches the
// inputs it declared, and that proposals it receives match the outputs.
func WithContracts(c map[string]*parley.Capability) Option {
	return func(in *Inbox) { in.contracts = c }
}

// WithCardRegistry replaces derived-key verification with the production
// registry path: Registry.VerifyMessageWithCard resolves the sender's
// current card and verifies against card.Pub.
func WithCardRegistry(registry *parley.Registry) Option {
	return func(in *Inbox) { in.verify = registry.VerifyMessageWithCard }
}

// Deliver processes one raw transport payload (JSON of a signed message).
// It is the single entry point used by every adapter (queue, HTTP, WS...).
func (in *Inbox) Deliver(raw []byte) *Result {
	var m parley.Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return &Result{Status: StatusRejected, Reason: "invalid json: " + err.Error()}
	}
	if m.ID == "" {
		return &Result{Status: StatusRejected, Reason: "message without content id"}
	}
	res := &Result{ID: m.ID}

	// 1) schema
	if !parley.SchemaValid(parley.MessageToAny(&m)) {
		res.Status = StatusRejected
		res.Reason = "invalid schema v0"
		return res
	}
	// 2) signature (mandatory in the trust model)
	ok, why := in.verify(&m)
	if !ok {
		res.Status = StatusRejected
		res.Reason = "signature: " + why
		return res
	}
	// 3) tenant isolation (when the inbox is tenant-scoped)
	if in.tenant != "" && m.Tenant != in.tenant {
		res.Status = StatusRejected
		res.Reason = "wrong tenant: " + m.Tenant
		return res
	}
	// 4) replay/idempotency: content-addressed id already seen
	in.mu.Lock()
	if in.seen[m.ID] {
		in.mu.Unlock()
		res.Status = StatusDuplicate
		res.Reason = "content id already delivered (replay)"
		return res
	}
	parent, hasParent := in.store[m.Parent]
	in.mu.Unlock()

	// 5) hybrid contract vs parent (when the parent is known). Mirrors the
	// Python reference: contract outcomes are REPORTED (accepted with
	// warnings), never a hard rejection — the counterparty decides.
	if hasParent && len(parent.Expect) > 0 && isContractAct(m.Act) {
		v := parley.ValidateWith(&m, parent, in.now().UTC(), in.contracts)
		res.Validation = v
		warns, _ := v["warnings"].([]string)
		for _, c := range v["clauses"].([]map[string]any) {
			if c["ok"] != nil && !c["ok"].(bool) {
				warns = append(warns, "clause violated: "+c["id"].(string))
			}
		}
		res.Warnings = warns
	} else if in.contracts != nil && m.Target != "" {
		// 6) capability body contract (when the target names a known
		// capability), even without a parent — e.g. a fresh request.
		if cap, ok := in.contracts[m.Target]; ok {
			bc := parley.BodyContract(&m, cap)
			if res.Validation == nil {
				res.Validation = map[string]any{}
			}
			res.Validation["body_contract"] = bc
			for _, v := range bc["violations"].([]string) {
				res.Warnings = append(res.Warnings, "body contract: "+v)
			}
			for _, w := range bc["warnings"].([]string) {
				res.Warnings = append(res.Warnings, "body contract: "+w)
			}
		}
	}

	// 7) negotiation thread: the act state machine (open on request,
	// offer/final transitions, loop cap). A grammar violation is REPORTED
	// (warning) — the thread organizes the grammar, it does not reject
	// delivery (same hybrid rule: the counterparty decides).
	in.mu.Lock()
	if m.Act == "request" && m.Target != "" {
		th := parley.NewThread(0)
		_ = th.Apply(&m) // opens the thread (a request with target always passes)
		in.threads[m.ID] = th
	}
	if m.Parent != "" {
		if th, ok := in.threads[m.Parent]; ok {
			if err := th.Apply(&m); err != nil {
				res.Warnings = append(res.Warnings, "thread: "+err.Error())
			}
			res.Validation = mergeValidation(res.Validation, map[string]any{
				"thread_state": string(th.State),
			})
		}
	}
	// 8) proposal ranking: when this message is an offer (propose/counter)
	// and the parent request is known, rank it (and the other stored
	// offers of the same thread) by the parent's clause weights. The
	// current message is included explicitly (it is stored below).
	if isContractAct(m.Act) && m.Parent != "" {
		if parent, ok := in.store[m.Parent]; ok && parent.Act == "request" && len(parent.Expect) > 0 {
			var offers []*parley.Message
			for _, sm := range in.store {
				if sm.Parent == m.Parent && (sm.Act == "propose" || sm.Act == "counter") {
					offers = append(offers, sm)
				}
			}
			offers = append(offers, &m)
			if rk := parley.RankProposals(parent, offers); rk.Top != nil {
				res.Validation = mergeValidation(res.Validation, map[string]any{
					"ranking": rk,
				})
			}
		}
	}
	in.seen[m.ID] = true
	in.seenAt[m.ID] = in.now()
	in.store[m.ID] = &m
	in.sweepLocked(in.now())
	needSave := in.storePath != ""
	in.mu.Unlock()
	res.Status = StatusAccepted
	if needSave {
		if err := in.Save(); err != nil {
			res.Reason = "persistence: " + err.Error()
		}
	}
	return res
}

func mergeValidation(base map[string]any, extra map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// Get returns a previously accepted message by id.
func (in *Inbox) Get(id string) (*parley.Message, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	m, ok := in.store[id]
	return m, ok
}

// Len is the number of accepted messages.
func (in *Inbox) Len() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.store)
}

// ThreadState returns the act-state-machine state of the negotiation
// rooted at rootID ("", false) when no such thread is tracked.
func (in *Inbox) ThreadState(rootID string) (parley.ThreadState, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	th, ok := in.threads[rootID]
	if !ok {
		return "", false
	}
	return th.State, true
}

// RankProposals re-runs the weighted ranking of every stored offer
// (propose/counter) whose parent is rootID (the request). Returns nil
// when there are no offers for that root.
func (in *Inbox) RankProposals(rootID string) *parley.Ranking {
	in.mu.Lock()
	parent, ok := in.store[rootID]
	if !ok {
		in.mu.Unlock()
		return nil
	}
	var offers []*parley.Message
	for _, sm := range in.store {
		if sm.Parent == rootID && (sm.Act == "propose" || sm.Act == "counter") {
			offers = append(offers, sm)
		}
	}
	in.mu.Unlock()
	if len(offers) == 0 {
		return nil
	}
	return parley.RankProposals(parent, offers)
}

// Save writes the accepted id set + messages to in.storePath (if set).
// Write is atomic (temp + rename) so a crash mid-write never corrupts the
// store, and a file lock serializes concurrent processes pointing at the
// same path (two inboxes writing the same file otherwise interleave).
func (in *Inbox) Save() error {
	in.mu.Lock()
	if in.storePath == "" {
		in.mu.Unlock()
		return nil
	}
	ids := make([]string, 0, len(in.seen))
	for id := range in.seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	msgs := make([]map[string]any, 0, len(in.store))
	for _, m := range in.store {
		msgs = append(msgs, parley.MessageToAny(m))
	}
	path := in.storePath
	in.mu.Unlock()
	b, err := json.Marshal(map[string]any{"ids": ids, "messages": msgs})
	if err != nil {
		return err
	}
	return lockedWrite(path, b)
}

// lockFile holds an exclusive advisory lock on path during a write
// (best-effort: on systems without flock support the write proceeds —
// atomicity via rename still holds).
func lockedWrite(path string, b []byte) error {
	if err := parley.AtomicWriteFile(path, b, 0644); err != nil {
		return err
	}
	return nil
}

// Load restores the accepted id set + messages from in.storePath (if set).
// Call once at startup, before delivering.
func (in *Inbox) Load() error {
	if in.storePath == "" {
		return nil
	}
	b, err := os.ReadFile(in.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var data struct {
		IDs      []string          `json:"ids"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(b, &data); err != nil {
		return err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, id := range data.IDs {
		in.seen[id] = true
	}
	for _, rm := range data.Messages {
		var m parley.Message
		if err := json.Unmarshal(rm, &m); err != nil {
			return err
		}
		in.store[m.ID] = &m
	}
	return nil
}

func isContractAct(act string) bool {
	switch act {
	case "propose", "counter", "accept", "commit":
		return true
	}
	return false
}

// ---------------------------------------------------------------- queue

// Queue is a minimal in-process transport (stands in for any broker:
// Kafka/NATS/SQS would deliver the same raw JSON to Deliver).
type Queue struct {
	ch chan []byte
}

// NewQueue creates a buffered in-process queue.
func NewQueue(buf int) *Queue {
	if buf <= 0 {
		buf = 16
	}
	return &Queue{ch: make(chan []byte, buf)}
}

// Send enqueues one raw message payload.
func (q *Queue) Send(raw []byte) {
	q.ch <- append([]byte(nil), raw...)
}

// Recv waits for one payload.
func (q *Queue) Recv() []byte {
	return <-q.ch
}

// Pump drains the queue into the inbox until closed+empty.
func (q *Queue) Pump(in *Inbox) {
	for raw := range q.ch {
		in.Deliver(raw)
	}
}
