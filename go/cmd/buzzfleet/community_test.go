package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

const legacy = `{
  "name": "agent-a",
  "workdir": "/srv/agent-a",
  "relay": "wss://relay-a.example",
  "agentCommand": "claude-agent-acp",
  "respondTo": "owner-only",
  "owner": "aa"
}
`

func fixtureHost(t *testing.T) *host.Host {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "agents", "agent-a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	return &host.Host{FleetHome: home}
}

func TestWriteCommunitiesRoundTrip(t *testing.T) {
	h := fixtureHost(t)
	cs := map[string]fleet.CommunityConfig{"b": {Relay: "wss://relay-b.example", Access: fleet.AccessFull, JoinedAt: 1}}
	if err := writeCommunities(h, "agent-a", cs); err != nil {
		t.Fatal(err)
	}
	raw, _ := h.RawAgentConfig("agent-a")
	if !strings.HasPrefix(string(raw), strings.TrimSuffix(strings.TrimSpace(legacy), "\n}")) {
		t.Fatalf("existing fields moved:\n%s", raw)
	}
	c, err := h.AgentConfig("agent-a")
	if err != nil || c.Communities["b"].Relay != "wss://relay-b.example" || c.Relay != "wss://relay-a.example" {
		t.Fatalf("%+v %v", c, err)
	}
	// An older binary reads the same main community from the same fields.
	var old struct{ Relay, Owner, RespondTo string }
	_ = json.Unmarshal(raw, &old)
	if old.Relay != "wss://relay-a.example" || old.Owner != "aa" {
		t.Fatalf("old reader: %+v", old)
	}
	if err := writeCommunities(h, "agent-a", nil); err != nil {
		t.Fatal(err)
	}
	back, _ := h.RawAgentConfig("agent-a")
	if strings.TrimSpace(string(back)) != strings.TrimSpace(legacy) {
		t.Fatalf("removing the last community must restore the file:\n%q", back)
	}
}

func TestResolveTargets(t *testing.T) {
	h := fixtureHost(t)
	ts, err := resolve(h, "agent-a")
	if err != nil || len(ts) != 1 || ts[0].Label != "agent-a" || ts[0].Instance() != "agent-a" {
		t.Fatalf("single community is shown as before: %+v %v", ts, err)
	}
	_ = writeCommunities(h, "agent-a", map[string]fleet.CommunityConfig{
		"b": {Relay: "wss://relay-b.example", Owner: "bb", Access: fleet.AccessFull}})
	ts, _ = resolve(h, "agent-a")
	if len(ts) != 2 || ts[0].Label != "agent-a@main" || ts[1].Label != "agent-a@b" || ts[1].Instance() != "agent-a--b" {
		t.Fatalf("got %+v", ts)
	}
	if ts[1].Relay != "wss://relay-b.example" || ts[1].Owner != "bb" {
		t.Fatalf("community settings: %+v", ts[1].Membership)
	}
	one, _ := resolve(h, "agent-a@b")
	if len(one) != 1 || one[0].Community != "b" {
		t.Fatalf("one: %+v", one)
	}
	main, _ := resolve(h, "agent-a@main")
	if len(main) != 1 || main[0].Instance() != "agent-a" {
		t.Fatal("@main addresses the original harness only, so a heal cannot restart the others")
	}
	if _, err := resolve(h, "agent-a@c"); err == nil || !strings.Contains(err.Error(), "main, b") {
		t.Fatalf("unknown community names the known ones: %v", err)
	}
}

func TestDropInLinks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := dropInDir("agent-a")
	put := func(p, body string) {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(body), 0o644)
	}
	put(filepath.Join(main, "model.conf"), "[Service]\nEnvironment=X=1\n")
	put(filepath.Join(main, "queue.conf"), "[Service]\n")
	made, err := linkDropIns("agent-a", "agent-a--b")
	if err != nil || len(made) != 2 {
		t.Fatalf("%v %v", made, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dropInDir("agent-a--b"), "model.conf")); string(b) != "[Service]\nEnvironment=X=1\n" {
		t.Fatal("the link reads the main drop-in")
	}
	// Idempotent.
	if again, err := linkDropIns("agent-a", "agent-a--b"); err != nil || len(again) != 0 {
		t.Fatalf("%v %v", again, err)
	}
	// Something put there by hand is never deleted.
	put(filepath.Join(dropInDir("agent-a--b"), "local.conf"), "[Service]\n")
	if err := unlinkDropIns("agent-a", "agent-a--b"); err == nil {
		t.Fatal("must refuse to remove a directory holding a real file")
	}
	_ = os.Remove(filepath.Join(dropInDir("agent-a--b"), "local.conf"))
	if err := unlinkDropIns("agent-a", "agent-a--b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dropInDir("agent-a--b")); !os.IsNotExist(err) {
		t.Fatal("directory left behind")
	}
	if _, err := os.Stat(filepath.Join(main, "model.conf")); err != nil {
		t.Fatal("the main drop-in must survive")
	}
}

func TestSplitFlags(t *testing.T) {
	f, pos := splitFlags([]string{"agent-a", "b", "--invite", "v2.x", "--access", "full", "--leave"})
	if len(pos) != 2 || f["invite"] != "v2.x" || f["access"] != "full" {
		t.Fatalf("%v %v", f, pos)
	}
	if _, ok := f["leave"]; !ok {
		t.Fatal("a bare flag is present with an empty value")
	}
}
