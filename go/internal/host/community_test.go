package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

func agentFixture(t *testing.T, body string) *Host {
	t.Helper()
	home := t.TempDir()
	h := &Host{FleetHome: home}
	put(t, filepath.Join(home, "agents", "agent-a", "agent.json"), body)
	return h
}

func TestLegacyAgentIsMainOnly(t *testing.T) {
	h := agentFixture(t, `{"name":"agent-a","workdir":"/srv/agent-a","relay":"wss://relay-a.example","owner":"aa","respondTo":"owner-only"}`)
	c, err := h.AgentConfig("agent-a")
	if err != nil {
		t.Fatal(err)
	}
	hs := c.Harnesses()
	if len(hs) != 1 || hs[0].Instance() != "agent-a" {
		t.Fatalf("a legacy agent is one harness with its old unit name: %+v", hs)
	}
	m, ok := c.In(fleet.MainCommunity)
	if !ok || m.Relay != "wss://relay-a.example" || m.Owner != "aa" {
		t.Fatalf("got %+v", m)
	}
	if _, ok := c.In("b"); ok {
		t.Fatal("unknown community")
	}
}

func TestCommunitySettingsFallBackToMain(t *testing.T) {
	h := agentFixture(t, `{"name":"agent-a","workdir":"/w","relay":"wss://relay-a.example","owner":"aa","respondTo":"owner-only",
	  "communities":{"c":{"relay":"wss://relay-c.example","access":"full"},"b":{"relay":"wss://relay-b.example","owner":"bb","respondTo":"anyone","access":"full"}}}`)
	c, _ := h.AgentConfig("agent-a")
	hs := c.Harnesses()
	if len(hs) != 3 || hs[0].Community != fleet.MainCommunity || hs[1].Community != "b" || hs[2].Community != "c" {
		t.Fatalf("main first, then by name: %+v", hs)
	}
	b, _ := c.In("b")
	if b.Relay != "wss://relay-b.example" || b.Owner != "bb" || b.RespondTo != "anyone" {
		t.Fatalf("b: %+v", b)
	}
	cc, _ := c.In("c")
	if cc.Owner != "aa" || cc.RespondTo != "owner-only" {
		t.Fatalf("c falls back to main's owner and reply mode: %+v", cc)
	}
	if got := len(h.AllHarnesses()); got != 3 {
		t.Fatalf("all harnesses: %d", got)
	}
}

func TestWriteRawAgentConfigKeepsMode(t *testing.T) {
	h := agentFixture(t, `{"name":"agent-a"}`)
	p := filepath.Join(h.FleetHome, "agents", "agent-a", "agent.json")
	_ = os.Chmod(p, 0o600)
	if err := h.WriteRawAgentConfig("agent-a", []byte(`{"name":"agent-a","x":1}`)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	b, _ := h.RawAgentConfig("agent-a")
	if fi.Mode().Perm() != 0o600 || string(b) != `{"name":"agent-a","x":1}` {
		t.Fatalf("mode %o body %s", fi.Mode().Perm(), b)
	}
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestDuplicateKeyIsIdentityAndRelay(t *testing.T) {
	a := Proc{Agent: "agent-a", Pubkey: "pk", Relay: "wss://relay-a.example"}
	b := Proc{Agent: "agent-a", Pubkey: "pk", Relay: "wss://relay-b.example"}
	if a.DuplicateKey() == b.DuplicateKey() {
		t.Fatal("one identity in two communities is not a duplicate")
	}
	if a.DuplicateKey() != (Proc{Agent: "other", Pubkey: "pk", Relay: "wss://relay-a.example"}).DuplicateKey() {
		t.Fatal("one identity twice on one relay is a duplicate, whatever it calls itself")
	}
	// Unreadable environment: fall back to what duplicates were keyed by
	// before communities existed.
	if (Proc{Agent: "agent-a"}).DuplicateKey() != (Proc{Agent: "agent-a"}).DuplicateKey() {
		t.Fatal("fallback")
	}
}
