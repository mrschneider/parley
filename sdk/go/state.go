// PARLEY v0 — Máquina de estados dos acts (thread de negociação).
//
// Os 11 acts do schema são os eventos de uma conversa; este arquivo dá a
// conversa um estado: uma Thread de negociação é aberta por um request
// (com target) e percorre os acts segundo uma gramática mínima, com
// terminal explícito (accept / commit / reject / cancel) e loops de
// negociação (propose/counter) com teto configurável.
//
// Semântica (espelha a referência Python, byte a byte em decisões):
//
//   - open:  só request com target (o contrato de uma negociação nasce de
//     um pedido; inform/ask não abrem thread)
//   - offer: propose ou counter; counter exige que já tenha havido oferta
//     anterior (não se "contra-oferece" o vazio)
//   - final: accept / commit / reject / cancel -> estado terminal; qualquer
//     ato subsequente é inválido (thread encerrada)
//   - reject/cancel também encerram a negotiation (não há "rejeito, e
//     depois...?")
//   - delegate: o agente reencaminha o assunto; a thread segue (o
//     reencaminhado responde), mas o ato é registrado como delegação
//   - inform/ask/verify: atos laterais — informam sem mudar o estado
//     (a thread não é sobre eles)
//   - loop cap: N propostas/counter consecutivas sem final -> o próximo
//     offer é inválido (negociações não terminam; o cap é um teto
//     pragmático, configurável)
//
// A Thread não fabrica confiança: ela só organiza a gramática. Assinatura,
// id de conteúdo e contrato híbrido continuam sendo o que dão garantia.
package parley

import "fmt"

// Estado da thread de negociação.
type ThreadState string

const (
	StateNew         ThreadState = "new"         // ainda não aberta
	StateNegotiating ThreadState = "negotiating" // em curso
	StateSettled     ThreadState = "settled"     // accept/commit (acordo)
	StateRejected    ThreadState = "rejected"    // reject/cancel (sem acordo)
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
		return fmt.Errorf("thread: mensagem nil")
	}
	switch t.State {
	case StateNew:
		return t.open(m)
	case StateNegotiating:
		return t.step(m)
	default: // settled / rejected
		return fmt.Errorf("thread %s: encerrada em %s — ato '%s' (%s) inválido",
			t.ID, t.State, m.Act, m.ID)
	}
}

func (t *Thread) open(m *Message) error {
	if m.Act != "request" || m.Target == "" {
		return fmt.Errorf("thread: open exige 'request' com target (obtido '%s' %s)", m.Act, m.ID)
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
			return fmt.Errorf("thread: 'counter' exige oferta anterior (thread %s, msg %s)", t.ID, m.ID)
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
		// o assunto segue com o reencaminhado; a thread não muda de estado
		t.trace(m)
	case "inform", "ask", "verify":
		// atos laterais: registrados, sem mudança de estado
		t.trace(m)
	default:
		return fmt.Errorf("thread: ato '%s' não reconhecido (msg %s)", m.Act, m.ID)
	}
	return nil
}

func (t *Thread) offer(m *Message) error {
	if t.OfferCount+1 > t.Cap {
		return fmt.Errorf("thread: teto de ofertas excedido (%d) — negociação sem final (thread %s, msg %s)",
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
