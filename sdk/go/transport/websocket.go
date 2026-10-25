// Package transport — WebSocket adapter.
//
// The envelope is exactly the same canonical JSON delivered by HTTP/queue;
// WebSocket only changes the framing. Each text message is one signed
// PARLEY envelope, and the server answers with the Inbox Result as one
// text JSON frame. This is the natural duplex path for negotiation
// counter-proposals (request -> propose -> counter -> accept).
package transport

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	parley "example.org/parley-go"
)

// WSServer adapts a WebSocket text-frame stream to Inbox.Deliver.
type WSServer struct {
	upgrader websocket.Upgrader
	mu       sync.RWMutex
	inboxes  map[string]*Inbox
}

// NewWSServer builds a WebSocket PARLEY endpoint. The default upgrader
// accepts same-origin requests only; production deployments should wrap
// Handler with authentication and explicit origin checks.
func NewWSServer() *WSServer {
	return &WSServer{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
		inboxes: map[string]*Inbox{},
	}
}

// AllowAllOrigins enables browser clients from any origin. Do not use this
// on a public endpoint unless combined with independent authentication.
func (s *WSServer) AllowAllOrigins() {
	s.upgrader.CheckOrigin = func(*http.Request) bool { return true }
}

// RegisterInbox binds inbox name to POST /inboxes/{name}.
func (s *WSServer) RegisterInbox(name string, in *Inbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inboxes[name] = in
}

// Handler implements http.Handler.
func (s *WSServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /inboxes/{name}", s.handleWS)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func (s *WSServer) inbox(name string) (*Inbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	in, ok := s.inboxes[name]
	return in, ok
}

func (s *WSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	in, ok := s.inbox(r.PathValue("name"))
	if !ok {
		http.Error(w, `{"error":"inbox not found"}`, http.StatusNotFound)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			_ = s.reply(conn, &Result{Status: StatusRejected, Reason: "text frames only (PARLEY JSON)"})
			continue
		}
		if err := s.reply(conn, in.Deliver(raw)); err != nil {
			return
		}
	}
}

func (*WSServer) reply(conn *websocket.Conn, res *Result) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, raw)
}

// WSSend dials endpoint/inbox, sends one signed PARLEY envelope as a text
// frame, and returns the Result frame. It closes the connection before
// returning; use a persistent client for repeated negotiation turns.
func WSSend(endpoint, inboxName string, m *parley.Message) (*Result, error) {
	conn, _, err := websocket.DefaultDialer.Dial(endpoint+"/inboxes/"+inboxName, nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		return nil, err
	}
	_, resultRaw, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var res Result
	if err := json.Unmarshal(resultRaw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
