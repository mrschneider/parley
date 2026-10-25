package transport

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	parley "example.org/parley-go"
)

func mkMessage(t *testing.T, ts string) *parley.Message {
	t.Helper()
	m := parley.NewMessage("a", "b", time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC))
	m.TS = ts
	m.Act = "inform"
	parley.AssignID(m)
	return parley.Sign(m, "a", "default")
}

func deliverOne(t *testing.T, in *Inbox, m *parley.Message) *Result {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return in.Deliver(raw)
}

func TestInboxRetentionAgeAndCount(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	in := NewInbox(WithRetention(10*time.Minute, 2))
	in.now = func() time.Time { return now }

	oldMsg := mkMessage(t, "2026-10-23T09:00:00Z")
	newer := []*parley.Message{
		mkMessage(t, "2026-10-23T09:55:00Z"),
		mkMessage(t, "2026-10-23T09:56:00Z"),
		mkMessage(t, "2026-10-23T09:57:00Z"),
	}
	for _, m := range append([]*parley.Message{oldMsg}, newer...) {
		if res := deliverOne(t, in, m); res.Status != StatusAccepted {
			t.Fatalf("deliver %s: %s %s", m.ID, res.Status, res.Reason)
		}
	}
	if in.Len() != 2 {
		t.Fatalf("after retention len=%d, want 2", in.Len())
	}
	if _, ok := in.Get(oldMsg.ID); ok {
		t.Fatal("expired message survived retention")
	}
	if _, ok := in.Get(newer[0].ID); ok {
		t.Fatal("oldest surviving record did not make room for newer records")
	}
	for _, m := range newer[1:] {
		if _, ok := in.Get(m.ID); !ok || !in.seen[m.ID] {
			t.Fatalf("recent record missing: %s", m.ID)
		}
	}
}

func TestInboxRetentionEvictedReplayCanBeAcceptedAgain(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	in := NewInbox(WithRetention(0, 1))
	in.now = func() time.Time { return now }
	old := mkMessage(t, "2026-10-23T09:59:00Z")
	newer := mkMessage(t, "2026-10-23T09:59:30Z")

	if res := deliverOne(t, in, old); res.Status != StatusAccepted {
		t.Fatalf("first delivery: %s %s", res.Status, res.Reason)
	}
	if res := deliverOne(t, in, old); res.Status != StatusDuplicate {
		t.Fatalf("replay before eviction: %s", res.Status)
	}
	if res := deliverOne(t, in, newer); res.Status != StatusAccepted {
		t.Fatalf("newer delivery: %s %s", res.Status, res.Reason)
	}
	if res := deliverOne(t, in, old); res.Status != StatusAccepted {
		t.Fatalf("evicted replay: %s %s", res.Status, res.Reason)
	}
}

func TestAtomicWriteSerializesConcurrentWriters(t *testing.T) {
	path := t.TempDir() + "/shared.json"
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := parley.AtomicWriteFile(path, []byte("writer"), 0644); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
