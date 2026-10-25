package parley

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeyStoreGenerateSignAndCardVerify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	ks, pubs, err := LoadOrGenerate(path, "acme", []string{"worker-1", "planner-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pubs) != 2 {
		t.Fatalf("expected 2 public keys, got %d", len(pubs))
	}

	card := &Card{
		V:         0,
		Agent:     "worker-1",
		Tenant:    "acme",
		Endpoint:  "https://worker.example",
		Transport: "http",
		Pub:       CardPubKey{Alg: "ed25519", Key: base64.StdEncoding.EncodeToString(pubs["worker-1"])},
	}
	card = SignCardWith(card, ks.MustGet("worker-1", "acme"))
	if ok, why := VerifyCard(card); !ok {
		t.Fatalf("card signature invalid: %s", why)
	}

	m := NewMessage("worker-1", "planner-1", time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "inform"
	AssignID(m)
	signed, err := ks.SignWith(m, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := VerifyWithKey(signed, pubs["worker-1"]); !ok {
		t.Fatalf("message signature invalid: %s", why)
	}

	reloaded := NewKeyStore()
	if err := reloaded.Load(path); err != nil {
		t.Fatal(err)
	}
	recovered, err := reloaded.SignWith(m, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Signature.Sig != signed.Signature.Sig {
		t.Fatal("reloaded key did not produce identical signature")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("keystore permissions = %v, want 0600", perm)
	}
}

func TestKeyStoreSignWithoutKeyFails(t *testing.T) {
	ks := NewKeyStore()
	m := NewMessage("ghost", "target", time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC))
	m.Tenant = "acme"
	m.Act = "inform"
	AssignID(m)
	if _, err := ks.SignWith(m, "acme"); err == nil {
		t.Fatal("expected error for missing key")
	}
}
