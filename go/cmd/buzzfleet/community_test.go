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

func TestGroupHarnessesByIdentityAndRelay(t *testing.T) {
	a1 := host.Proc{PID: "1", Agent: "agent-a", Pubkey: "pk", Relay: "wss://relay-a.example", Supervised: true}
	b1 := host.Proc{PID: "2", Agent: "agent-a", Pubkey: "pk", Relay: "wss://relay-b.example", Supervised: true}
	keys, by := groupHarnesses([]host.Proc{a1, b1})
	if len(keys) != 2 {
		t.Fatalf("one identity in two communities is two groups of one: %v", by)
	}
	stray := host.Proc{PID: "3", Agent: "agent-a", Pubkey: "pk", Relay: "wss://relay-b.example"}
	keys, by = groupHarnesses([]host.Proc{a1, b1, stray})
	if len(keys) != 2 || len(by[keys[1]]) != 2 {
		t.Fatalf("the same identity twice on one relay is a duplicate: %v", by)
	}
	// Started by hand with flags, so no relay in its environment: it must
	// still be counted against the identity, not hide as a group of one.
	flags := host.Proc{PID: "4", Agent: "agent-a", Pubkey: "pk"}
	keys, by = groupHarnesses([]host.Proc{a1, flags})
	if len(keys) != 1 || len(by[keys[0]]) != 2 {
		t.Fatalf("relay unknown: %v", by)
	}
	noenv := host.Proc{PID: "5", Agent: "agent-a"}
	keys, _ = groupHarnesses([]host.Proc{a1, noenv})
	if len(keys) != 1 {
		t.Fatal("no environment at all: grouped by agent")
	}
}

func TestHealTargetsOnlyTheWedgedHarness(t *testing.T) {
	h := fixtureHost(t)
	_ = writeCommunities(h, "agent-a", map[string]fleet.CommunityConfig{
		"b": {Relay: "wss://relay-b.example", Access: fleet.AccessFull}})
	ts, _ := resolve(h, "agent-a")
	wedged := assessTarget(ts[1], "active", fleet.TriNo, fleet.TriUnknown, true)
	if !wedged.NeedsHeal() {
		t.Fatalf("absent from b's relay: %+v", wedged)
	}
	// What the supervisor restarts is whatever the assessment's name
	// resolves to.
	got, err := resolve(h, wedged.Name)
	if err != nil || len(got) != 1 || got[0].Instance() != "agent-a--b" {
		t.Fatalf("a wedge in b must restart b alone, got %+v (%v)", got, err)
	}
	ok := assessTarget(ts[0], "active", fleet.TriYes, fleet.TriYes, true)
	if got, _ := resolve(h, ok.Name); len(got) != 1 || got[0].Instance() != "agent-a" {
		t.Fatalf("and main resolves to main alone: %+v", got)
	}
}

func TestHarnessPromptForAMultiCommunityAgent(t *testing.T) {
	h := fixtureHost(t)
	_ = writeCommunities(h, "agent-a", map[string]fleet.CommunityConfig{
		"b": {Relay: "wss://relay-b.example", Access: fleet.AccessFull}})
	dir := filepath.Join(h.FleetHome, "agents", "agent-a")
	_ = os.WriteFile(filepath.Join(dir, "system-prompt.md"), []byte("You are the base persona.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "system-prompt.b.md"), []byte("In b you are also its research assistant.\n"), 0o644)
	c, _ := h.AgentConfig("agent-a")

	path, env, err := harnessPrompt(h, c, fleet.Harness{Agent: "agent-a", Community: "b"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	s := string(b)
	base, add, sec := strings.Index(s, "base persona"), strings.Index(s, "research assistant"), strings.Index(s, "## Your communities")
	if base < 0 || add < base || sec < add {
		t.Fatalf("personality, then the community's addendum, then where it is:\n%s", s)
	}
	if strings.Contains(s, "relay-b.example") {
		t.Fatal("no relay address in the prompt")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("generated prompt mode %o", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "shared-notes.md")); err != nil {
		t.Fatal("the shared notes file is created")
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, `BUZZ_FLEET_COMMUNITIES=[{"name":"main","current":false},{"name":"b","current":true}]`) ||
		!strings.Contains(joined, "BUZZ_FLEET_SHARED_NOTES="+filepath.Join(dir, "shared-notes.md")) {
		t.Fatalf("env: %v", env)
	}

	// main has no addendum: personality and section only, and it is told it
	// is answering in main.
	path, _, _ = harnessPrompt(h, c, fleet.Harness{Agent: "agent-a", Community: fleet.MainCommunity})
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "research assistant") || !strings.Contains(string(b), "answering in **main** now") {
		t.Fatalf("main's prompt:\n%s", b)
	}
	// An existing notes file is never overwritten.
	_ = os.WriteFile(filepath.Join(dir, "shared-notes.md"), []byte("kept\n"), 0o640)
	_, _, _ = harnessPrompt(h, c, fleet.Harness{Agent: "agent-a", Community: "b"})
	if b, _ := os.ReadFile(filepath.Join(dir, "shared-notes.md")); string(b) != "kept\n" {
		t.Fatal("notes overwritten")
	}
}

func TestPeekEnvCarriesOnlyWhatItNeeds(t *testing.T) {
	from := []string{"PATH=/bin", "HOME=/h", "BUZZ_AUTH_TAG=[\"auth\",\"owner\",\"\",\"sig\"]",
		"BUZZ_RELAY_URL=https://relay-b.example", "BUZZ_PRIVATE_KEY=harness-key", "BUZZ_FLEET_HARNESS=agent-a--b",
		"BUZZ_OUTPUT_FORMAT=compact", "ANTHROPIC_API_KEY=x"}
	env := strings.Join(peekEnv(from, "https://relay-a.example", "agent-key"), "\n")
	for _, gone := range []string{"BUZZ_AUTH_TAG", "relay-b.example", "harness-key", "ANTHROPIC_API_KEY", "BUZZ_FLEET_HARNESS"} {
		if strings.Contains(env, gone) {
			t.Fatalf("%s must not reach the other community's relay:\n%s", gone, env)
		}
	}
	for _, kept := range []string{"PATH=/bin", "HOME=/h", "BUZZ_OUTPUT_FORMAT=compact", "BUZZ_RELAY_URL=https://relay-a.example", "BUZZ_PRIVATE_KEY=agent-key"} {
		if !strings.Contains(env, kept) {
			t.Fatalf("missing %s:\n%s", kept, env)
		}
	}
}

func TestPeekReadsOnlyAsTheAgent(t *testing.T) {
	h := fixtureHost(t)
	agentKey := strings.Repeat("0", 63) + "1"
	ownerKey := strings.Repeat("0", 63) + "2"
	_ = os.WriteFile(filepath.Join(h.FleetHome, "agents", "agent-a", "identity.env"), []byte("BUZZ_PRIVATE_KEY="+agentKey+"\n"), 0o600)

	if k, err := peekSecret(h, "agent-a", true, ownerKey); err != nil || k != agentKey {
		t.Fatalf("operator mode must use the agent's key, not the caller's: %q %v", k, err)
	}
	if _, err := peekSecret(h, "agent-a", false, ownerKey); err == nil {
		t.Fatal("inside a harness, a key that is not the agent's is refused")
	}
	if k, err := peekSecret(h, "agent-a", false, agentKey); err != nil || k != agentKey {
		t.Fatalf("the agent's own key: %q %v", k, err)
	}
	if k, _ := peekSecret(h, "agent-a", false, ""); k != agentKey {
		t.Fatal("no key in the environment: the agent's from its identity file")
	}
}

func TestPeekInsideAHarnessIsThatAgentOnly(t *testing.T) {
	if peekAgentAllowed("agent-a", "agent-b", true) == nil {
		t.Fatal("one agent must not peek as another")
	}
	if peekAgentAllowed("agent-a", "agent-a", true) != nil || peekAgentAllowed("", "agent-b", true) != nil || peekAgentAllowed("agent-a", "", false) != nil {
		t.Fatal("its own agent, or an operator outside any harness, is fine")
	}
}

func TestMemFlagsRepeat(t *testing.T) {
	f, pos := memFlags([]string{"agent-a", "--exclude", "mem/p/**", "--exclude", "mem/q", "--both", "--resolve", "mem/a=from", "--dry-run"})
	if len(pos) != 1 || len(f["exclude"]) != 2 || len(f["both"]) != 1 || f["resolve"][0] != "mem/a=from" || len(f["dry-run"]) != 1 {
		t.Fatalf("%v %v", f, pos)
	}
}

func TestPrivateFilesAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory-conflicts", "main--b")
	p := filepath.Join(dir, "mem__a.json")
	if err := writePrivate(p, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("file %o dir %o: they hold decrypted memory", fi.Mode().Perm(), di.Mode().Perm())
	}
}
