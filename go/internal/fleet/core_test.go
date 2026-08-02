package fleet

import (
	"strings"
	"testing"
)

func cfg() *Config {
	return (&Config{
		Name:      "fleet-clawd",
		Broadcast: "fleet",
		PublicKey: strings.Repeat("f", 64),
		Owner:     strings.Repeat("a", 64),
	}).WithDefaults()
}

func msg(content string, opts ...func(*Message)) Message {
	m := Message{ID: "i1", PubKey: strings.Repeat("a", 64), Content: content}
	for _, o := range opts {
		o(&m)
	}
	return m
}

// ── addressing ────────────────────────────────────────────────────────────────

func TestAddressedByOwnName(t *testing.T) {
	for _, in := range []string{
		"@fleet-clawd status", "fleet-clawd status", "fleet-clawd: status", "fleet-clawd, status",
	} {
		c, ok := ParseCommand(msg(in), cfg())
		if !ok || c.Text != "status" {
			t.Fatalf("%q -> %+v ok=%v", in, c, ok)
		}
	}
}

func TestBroadcastNameReachesEveryHost(t *testing.T) {
	c, ok := ParseCommand(msg("@fleet status"), cfg())
	if !ok || c.Text != "status" || c.AddressedTo != "fleet" {
		t.Fatalf("got %+v ok=%v", c, ok)
	}
}

func TestPTagMentionCounts(t *testing.T) {
	m := msg("@somebody restart charles", func(m *Message) { m.PTags = []string{strings.Repeat("f", 64)} })
	c, ok := ParseCommand(m, cfg())
	if !ok || c.Text != "restart charles" {
		t.Fatalf("got %+v ok=%v", c, ok)
	}
}

func TestMessagesForOthersIgnored(t *testing.T) {
	for _, in := range []string{"hey charles can you look at this", ""} {
		if _, ok := ParseCommand(msg(in), cfg()); ok {
			t.Fatalf("%q should not be treated as a command", in)
		}
	}
}

func TestBareNameAsksForHelp(t *testing.T) {
	c, ok := ParseCommand(msg("@fleet"), cfg())
	if !ok || c.Text != "help" {
		t.Fatalf("got %+v ok=%v", c, ok)
	}
}

func TestRegexLikeNameIsLiteral(t *testing.T) {
	c := cfg()
	c.Name, c.Broadcast = "fleet.a+b", ""
	if got, ok := ParseCommand(msg("fleet.a+b status"), c); !ok || got.Text != "status" {
		t.Fatalf("literal name should match: %+v", got)
	}
	if _, ok := ParseCommand(msg("fleetXaXb status"), c); ok {
		t.Fatal("name must not be interpreted as a regex")
	}
}

func TestParseVerb(t *testing.T) {
	v, args := ParseVerb("  LOGS charles 40 ")
	if v != "logs" || len(args) != 2 || args[0] != "charles" {
		t.Fatalf("got %q %v", v, args)
	}
	if v, _ := ParseVerb(""); v != "" {
		t.Fatalf("empty should stay empty, got %q", v)
	}
}

// ── authorisation and replay ──────────────────────────────────────────────────

func TestOnlyOwnerIsObeyed(t *testing.T) {
	seen := map[string]bool{}
	if !ShouldHandle(msg("x"), seen, cfg()) {
		t.Fatal("owner should be obeyed")
	}
	stranger := msg("x", func(m *Message) { m.PubKey = strings.Repeat("b", 64) })
	if ShouldHandle(stranger, seen, cfg()) {
		t.Fatal("a non-owner must not be able to restart agents")
	}
}

func TestNeverAnswersItself(t *testing.T) {
	self := msg("x", func(m *Message) { m.PubKey = strings.Repeat("f", 64) })
	if ShouldHandle(self, map[string]bool{}, cfg()) {
		t.Fatal("supervisor must ignore its own messages")
	}
}

func TestCommandNeverRunsTwice(t *testing.T) {
	seen := map[string]bool{}
	m := msg("status")
	if !ShouldHandle(m, seen, cfg()) {
		t.Fatal("first time should run")
	}
	seen[m.ID] = true
	if ShouldHandle(m, seen, cfg()) {
		t.Fatal("a command must never execute twice")
	}
}

// ── health assessment ─────────────────────────────────────────────────────────

func TestAssessStates(t *testing.T) {
	cases := []struct {
		name     string
		unit     string
		present  Tri
		child    Tri
		identity bool
		want     State
	}{
		{"stopped unit is down", "inactive", TriNo, TriUnknown, true, StateDown},
		{"absent from relay is wedged", "active", TriNo, TriYes, true, StateWedged},
		{"unreachable relay is unknown", "active", TriUnknown, TriUnknown, true, StateUnknown},
		{"no agent process is stalled", "active", TriYes, TriNo, true, StateStalled},
		{"healthy is ok", "active", TriYes, TriYes, true, StateOK},
		{"missing identity is unknown", "active", TriYes, TriYes, false, StateUnknown},
	}
	for _, c := range cases {
		got := Assess("a", c.unit, c.present, c.child, c.identity)
		if got.State != c.want {
			t.Errorf("%s: got %q want %q", c.name, got.State, c.want)
		}
	}
}

func TestUnknownIsNeverHealed(t *testing.T) {
	// Our own connectivity failure must not restart a healthy agent.
	a := Assess("a", "active", TriUnknown, TriUnknown, true)
	if a.NeedsHeal() {
		t.Fatal("an unreachable relay must never trigger a restart")
	}
}

// ── circuit breaker ───────────────────────────────────────────────────────────

func wedged() Assessment {
	return Assessment{Name: "a", State: StateWedged, Detail: "running but absent from relay"}
}

func TestOneBadReadingIsNotEnough(t *testing.T) {
	d := Decide("a", wedged(), 0, map[string][]int64{}, 100, cfg())
	if d.Action != ActionStrike || d.Strikes != 1 {
		t.Fatalf("got %+v", d)
	}
}

func TestTwoConsecutiveReadingsHeal(t *testing.T) {
	if d := Decide("a", wedged(), 1, map[string][]int64{}, 100, cfg()); d.Action != ActionHeal {
		t.Fatalf("got %+v", d)
	}
}

func TestRecoveryClearsStrikes(t *testing.T) {
	ok := Assessment{Name: "a", State: StateOK}
	if d := Decide("a", ok, 1, map[string][]int64{}, 100, cfg()); d.Action != ActionClear {
		t.Fatalf("got %+v", d)
	}
}

func TestStalledHealsToo(t *testing.T) {
	st := Assessment{Name: "a", State: StateStalled}
	if d := Decide("a", st, 1, map[string][]int64{}, 100, cfg()); d.Action != ActionHeal {
		t.Fatalf("got %+v", d)
	}
}

// The bug this exists for: without a breaker, a permanently broken agent is
// restarted every couple of minutes forever.
func TestCircuitBreakerStopsRestartLoop(t *testing.T) {
	history := map[string][]int64{}
	c := cfg()
	var now int64 = 1000
	for i := 0; i < c.MaxHealsPerHr; i++ {
		if d := Decide("a", wedged(), 1, history, now, c); d.Action != ActionHeal {
			t.Fatalf("restart %d should be allowed, got %+v", i+1, d)
		}
		history["a"] = append(history["a"], now)
		now += 130
	}
	d := Decide("a", wedged(), 1, history, now, c)
	if d.Action != ActionGiveUp {
		t.Fatalf("the next attempt must stop, got %+v", d)
	}
	if !strings.Contains(d.Reason, "restarted 3 times") || !strings.Contains(d.Reason, "logs a") {
		t.Fatalf("the message should say what happened and what to do: %q", d.Reason)
	}
}

func TestBreakerReArmsAfterWindow(t *testing.T) {
	history := map[string][]int64{"a": {1000, 1100, 1200}}
	later := int64(1200 + 3601) // past the newest heal, not merely the oldest
	if n := HealsInWindow(history, "a", later, 3600); n != 0 {
		t.Fatalf("window should be empty, got %d", n)
	}
	if d := Decide("a", wedged(), 1, history, later, cfg()); d.Action != ActionHeal {
		t.Fatalf("got %+v", d)
	}
}

func TestBreakerIsPerAgent(t *testing.T) {
	history := map[string][]int64{"a": {1000, 1100, 1200}}
	if d := Decide("b", wedged(), 1, history, 1300, cfg()); d.Action != ActionHeal {
		t.Fatalf("a healthy agent must not be punished for a broken sibling: %+v", d)
	}
}

func TestPruneHeals(t *testing.T) {
	history := map[string][]int64{"a": {10, 20}, "b": {5000}}
	PruneHeals(history, 6000, 3600)
	if _, ok := history["a"]; ok {
		t.Fatal("aged-out entries should be dropped")
	}
	if len(history["b"]) != 1 {
		t.Fatalf("recent entries should stay: %v", history["b"])
	}
}

// ── reply placement ───────────────────────────────────────────────────────────

func TestShortRepliesStayInline(t *testing.T) {
	// A status table is the common case and must not cost an extra tap.
	short := "**clawd**\n```\nAGENT  STATE\ncharles active\n\ncharles ok\n```"
	if ShouldThread(short) {
		t.Fatalf("a short reply should be inline: %d lines", strings.Count(short, "\n")+1)
	}
}

func TestLongRepliesAreThreaded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("Aug 01 15:00:00 clawd buzz-acp[1]: some journal line\n")
	}
	if !ShouldThread(b.String()) {
		t.Fatal("a screenful of logs should be threaded")
	}
}

func TestWideRepliesAreThreadedEvenIfFewLines(t *testing.T) {
	// Few newlines but very long still floods a phone screen.
	if !ShouldThread(strings.Repeat("x", 1000)) {
		t.Fatal("a very long single-line reply should be threaded")
	}
}
