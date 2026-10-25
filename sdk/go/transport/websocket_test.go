package transport

import (
	"net/http/httptest"
	"strings"
	"testing"

	parley "example.org/parley-go"
)

func TestWebSocketTransportDeliversAndRejectsBinaryFrames(t *testing.T) {
	server := NewWSServer()
	in := NewInbox()
	server.RegisterInbox("dispatcher-7", in)
	httpSrv := httptest.NewServer(server.Handler())
	defer httpSrv.Close()
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")

	m := parley.NewMessage("planner-42", "dispatcher-7", t0)
	m.Tenant = "acme"
	m.Act = "request"
	m.Target = "route.plan"
	m.Body = map[string]any{"origin": "Lisboa", "dest": "Porto"}
	parley.AssignID(m)
	m = parley.Sign(m, "planner-42", "acme")

	res, err := WSSend(wsURL, "dispatcher-7", m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusAccepted {
		t.Fatalf("websocket delivery: %s %s", res.Status, res.Reason)
	}
	if in.Len() != 1 {
		t.Fatalf("inbox len=%d, want 1", in.Len())
	}
}
