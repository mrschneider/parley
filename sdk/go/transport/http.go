package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"example.org/parley-go"
)

// HTTPServer is a live PARLEY v0 transport: agents POST raw signed message
// JSON to POST /inboxes/{name}; the server delivers to the matching inbox
// and returns the delivery Result as JSON. The envelope is identical to
// what a queue would deliver — the transport is a detail, the protocol is
// the canonical JSON.
type HTTPServer struct {
	mu      sync.RWMutex
	inboxes map[string]*Inbox
}

func NewHTTPServer() *HTTPServer {
	return &HTTPServer{inboxes: map[string]*Inbox{}}
}

// RegisterInbox binds an inbox name to a handler.
func (s *HTTPServer) RegisterInbox(name string, in *Inbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inboxes[name] = in
}

// Handler implements http.Handler.
func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /inboxes/{name}", s.handlePost)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	return mux
}

func (s *HTTPServer) handlePost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.RLock()
	in, ok := s.inboxes[name]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"inbox não encontrado"}`, http.StatusNotFound)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"leitura do corpo falhou"}`, http.StatusBadRequest)
		return
	}
	res := in.Deliver(raw)
	status := http.StatusAccepted
	switch res.Status {
	case StatusRejected:
		status = http.StatusUnprocessableEntity
	case StatusDuplicate:
		status = http.StatusOK // idempotent: same decision, no side effects
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// Send delivers one signed message to an inbox over the live HTTP transport
// (used by tests/demos to exercise the full network path).
func (s *HTTPServer) Send(baseURL, inbox string, m *parley.Message) (*Result, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(baseURL+"/inboxes/"+inbox, "application/json",
		bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var res Result
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	return &res, nil
}
