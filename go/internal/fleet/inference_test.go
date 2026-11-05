package fleet

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixtures are synthetic. They keep only the structural fields the parsers
// read, in the shapes Claude Code and Codex write.

const (
	claudeOK = `{"type":"assistant","timestamp":"2026-01-10T12:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`

	claudeWeekly = `{"type":"assistant","timestamp":"2026-01-10T12:05:00.000Z","isApiErrorMessage":true,"error":"rate_limit",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your weekly limit · resets Jan 12, 10pm (UTC)"}]}}`

	claudeSession = `{"type":"assistant","timestamp":"2026-01-10T12:05:00.000Z","isApiErrorMessage":true,"error":"rate_limit",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your session limit · resets 7:10pm (UTC)"}]}}`

	claudeSpend = `{"type":"assistant","timestamp":"2026-01-10T12:05:00.000Z","isApiErrorMessage":true,"error":"rate_limit",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"You've hit your monthly spend limit · raise it in settings"}]}}`

	claudeAuth = `{"type":"assistant","timestamp":"2026-01-10T12:06:00.000Z","isApiErrorMessage":true,"error":"authentication_failed",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"Failed to authenticate: OAuth session expired and could not be refreshed"}]}}`

	claudeCompactLimit = `{"type":"assistant","timestamp":"2026-01-10T12:07:00.000Z","isApiErrorMessage":true,"error":"invalid_request",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"Prompt is too long · automatic compaction failed: You've hit your weekly limit · resets 10pm (UTC)"}]}}`

	claudeTooLong = `{"type":"assistant","timestamp":"2026-01-10T12:08:00.000Z","isApiErrorMessage":true,"error":"invalid_request",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"Prompt is too long"}]}}`

	claudeUser    = `{"type":"user","timestamp":"2026-01-10T12:09:00.000Z","message":{"role":"user","content":"You've hit your weekly limit"}}`
	claudeSubtask = `{"type":"assistant","isSidechain":true,"timestamp":"2026-01-10T12:10:00.000Z","message":{"content":[]}}`
)

func TestClaudeRecordsClassified(t *testing.T) {
	cases := []struct {
		name string
		line string
		kind SignalKind
		ok   bool
	}{
		{"normal turn", claudeOK, SigOK, true},
		{"weekly limit", claudeWeekly, SigLimited, true},
		{"session limit", claudeSession, SigLimited, true},
		{"monthly spend", claudeSpend, SigLimited, true},
		{"expired login", claudeAuth, SigAuth, true},
		{"limit reported via compaction", claudeCompactLimit, SigLimited, true},
		{"prompt too long is not an account fault", claudeTooLong, SigNone, false},
		{"user text quoting a limit is not a signal", claudeUser, SigNone, false},
		{"subagent turns do not decide the agent's state", claudeSubtask, SigNone, false},
		{"garbage", `{not json`, SigNone, false},
	}
	for _, c := range cases {
		s, ok := ParseClaudeLine([]byte(c.line))
		if ok != c.ok || s.Kind != c.kind {
			t.Fatalf("%s: got kind=%d ok=%v", c.name, s.Kind, ok)
		}
	}
}

func codexUsage(used float64, extra string) string {
	return `{"timestamp":"2026-01-10T12:00:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{},` +
		`"rate_limits":{"primary":{"used_percent":` + strconv.FormatFloat(used, 'f', -1, 64) + `,"window_minutes":300,"resets_at":1768060800},` +
		`"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"rate_limit_reached_type":null` + extra + `}}}`
}

func TestCodexLimitsComeFromTheUsageSnapshot(t *testing.T) {
	s, ok := ParseCodexLine([]byte(codexUsage(97, "")))
	if !ok || s.Kind != SigOK {
		t.Fatalf("97%% used: the model answered, got %+v", s)
	}
	s, ok = ParseCodexLine([]byte(codexUsage(100, "")))
	if !ok || s.Kind != SigLimited || !s.Resets.Equal(time.Unix(1768060800, 0)) {
		t.Fatalf("100%% used: limited until the window resets, got %+v", s)
	}
	reached := strings.Replace(codexUsage(40, ""), `"rate_limit_reached_type":null`, `"rate_limit_reached_type":"usage_limit_reached"`, 1)
	if s, _ := ParseCodexLine([]byte(reached)); s.Kind != SigLimited {
		t.Fatalf("a reached-limit marker is limited whatever the percentage: %+v", s)
	}
	credits := strings.Replace(codexUsage(100, ""), `"has_credits":false`, `"has_credits":true`, 1)
	if s, _ := ParseCodexLine([]byte(credits)); s.Kind != SigOK {
		t.Fatalf("past 100%% with credits the account carries on: %+v", s)
	}
}

func TestCodexIgnoresTurnEndsAndToolOutput(t *testing.T) {
	for _, line := range []string{
		// Written after failed turns too, so it proves nothing.
		`{"timestamp":"2026-01-10T12:00:00.000Z","type":"event_msg","payload":{"type":"task_complete"}}`,
		// Never persisted by Codex, but must not be trusted if it ever is.
		`{"timestamp":"2026-01-10T12:01:00.000Z","type":"event_msg","payload":{"type":"error","message":"401 Unauthorized"}}`,
		`{"timestamp":"2026-01-10T12:04:00.000Z","type":"response_item","payload":{"type":"function_call_output","output":"curl: 401 Unauthorized"}}`,
		// A token count with no usage snapshot says nothing about the account.
		`{"timestamp":"2026-01-10T12:05:00.000Z","type":"event_msg","payload":{"type":"token_count","info":{}}}`,
	} {
		if _, ok := ParseCodexLine([]byte(line)); ok {
			t.Fatalf("must not be a signal: %s", line)
		}
	}
	// Limited, then the turn "completes": still limited.
	lim, _ := ParseCodexLine([]byte(codexUsage(100, "")))
	if st, _ := InferenceState(Latest([]Signal{lim}), lim.At.Add(time.Minute)); st != StateLimited {
		t.Fatalf("got %s", st)
	}
}

func TestParseReset(t *testing.T) {
	at := time.Date(2026, 1, 10, 18, 34, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"resets Jan 12, 10pm (UTC)":     time.Date(2026, 1, 12, 22, 0, 0, 0, time.UTC),
		"resets 7:10pm (UTC)":           time.Date(2026, 1, 10, 19, 10, 0, 0, time.UTC),
		"resets 6pm (UTC)":              time.Date(2026, 1, 11, 18, 0, 0, 0, time.UTC), // already past today
		"resets 12am (UTC)":             time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC),
		"try again in 1 hour 5 minutes": at.Add(65 * time.Minute),
	}
	for in, want := range cases {
		got, ok := ParseReset(in, at)
		if !ok || !got.Equal(want) {
			t.Fatalf("%q: got %v ok=%v want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseReset("raise it in settings", at); ok {
		t.Fatal("no reset in the text must not invent one")
	}
	// Written in late December about early January.
	dec := time.Date(2026, 12, 30, 10, 0, 0, 0, time.UTC)
	if got, _ := ParseReset("resets Jan 2, 1am (UTC)", dec); got.Year() != 2027 {
		t.Fatalf("year rollover: %v", got)
	}
}

func TestLatestTurnDecides(t *testing.T) {
	sig := func(line string) Signal { s, _ := ParseClaudeLine([]byte(line)); return s }
	// An expired login, then someone logged in again and a turn worked.
	recovered := []Signal{sig(claudeAuth), sig(strings.Replace(claudeOK, "12:00:00", "12:30:00", 1))}
	if st, _ := InferenceState(Latest(recovered), time.Now()); st != StateOK {
		t.Fatalf("a good turn after the failure must clear it, got %s", st)
	}
	broken := []Signal{sig(claudeOK), sig(claudeAuth)}
	st, detail := InferenceState(Latest(broken), time.Now())
	if st != StateAuth || !strings.Contains(detail, "OAuth session expired") {
		t.Fatalf("got %s %q", st, detail)
	}
}

func TestLimitLiftsAtReset(t *testing.T) {
	s, _ := ParseClaudeLine([]byte(claudeWeekly))
	before := time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC)
	st, detail := InferenceState(s, before)
	if st != StateLimited || detail != "usage limit until Jan 12 22:00 UTC" {
		t.Fatalf("got %s %q", st, detail)
	}
	if st, _ := InferenceState(s, s.Resets.Add(time.Minute)); st != StateOK {
		t.Fatalf("past the reset the agent should be tried again, got %s", st)
	}
	// No reset in the message: limited until a good turn says otherwise.
	spend, _ := ParseClaudeLine([]byte(claudeSpend))
	if st, d := InferenceState(spend, before.AddDate(0, 1, 0)); st != StateLimited || d != "usage limit: You've hit your monthly spend limit" {
		t.Fatalf("got %s %q", st, d)
	}
}

func TestInferenceNeverHealsAndRelayFaultsComeFirst(t *testing.T) {
	ok := Assessment{Name: "agent-a", State: StateOK}
	if a := WithInference(ok, StateLimited, "x"); a.State != StateLimited || a.NeedsHeal() {
		t.Fatalf("limited must be shown and never restarted: %+v", a)
	}
	wedged := Assessment{Name: "agent-a", State: StateWedged}
	if a := WithInference(wedged, StateAuth, "x"); a.State != StateWedged {
		t.Fatal("a wedge is still healed: a restart fixes that part")
	}
	h := map[string][]int64{}
	if d := Decide("agent-a", WithInference(ok, StateAuth, "x"), 5, h, 0, cfg()); d.Action == ActionHeal || d.Action == ActionGiveUp {
		t.Fatalf("auth must not restart-loop: %+v", d)
	}
}

func TestInferenceNoticesGroupedAndOnce(t *testing.T) {
	lim := func(n string) Assessment {
		return Assessment{Name: n, State: StateLimited, Detail: "usage limit until Jan 12 22:00 UTC"}
	}
	rows := []Assessment{lim("agent-a"), lim("agent-b"), lim("agent-c"),
		{Name: "agent-d", State: StateAuth, Detail: "model login failed: expired"},
		{Name: "agent-e", State: StateOK}}

	notices, state := InferenceNotices("host-1", map[string]string{}, rows)
	if len(notices) != 2 {
		t.Fatalf("one message per cause, not per agent: %q", notices)
	}
	if !strings.Contains(notices[0], "**agent-a**, **agent-b**, **agent-c** are out of model usage") ||
		!strings.Contains(notices[0], "not restarting them") {
		t.Fatalf("got %q", notices[0])
	}
	if !strings.Contains(notices[1], "**agent-d** cannot log in to its model") {
		t.Fatalf("got %q", notices[1])
	}

	again, state := InferenceNotices("host-1", state, rows)
	if len(again) != 0 {
		t.Fatalf("must be said once, got %q", again)
	}

	// The relay blipped: agent-a is unknown this tick. That is not a recovery.
	rows[0] = Assessment{Name: "agent-a", State: StateUnknown}
	if n, s := InferenceNotices("host-1", state, rows); len(n) != 0 || s["agent-a"] == "" {
		t.Fatalf("unknown must neither announce recovery nor forget: %q %v", n, s)
	} else {
		state = s
	}

	rows[0], rows[1] = Assessment{Name: "agent-a", State: StateOK}, Assessment{Name: "agent-b", State: StateOK}
	rows[2] = WithInference(Assessment{Name: "agent-c", State: StateOK}, StateOK, "limit should have reset at Jan 12 22:00 UTC")
	n, state := InferenceNotices("host-1", state, rows)
	if len(n) != 2 || !strings.Contains(n[0], "**agent-c** is past its usage reset") ||
		!strings.Contains(n[1], "**agent-a**, **agent-b** are answering normally again") {
		t.Fatalf("a reset with no turn since is not a recovery: %q", n)
	}
	if _, still := state["agent-a"]; still {
		t.Fatal("recovered agents leave the state")
	}
}

func TestTranscriptOwnership(t *testing.T) {
	wd := []string{"/srv/agent", "/srv/agent-b", "/srv/other"}
	cases := map[string]string{
		"-srv-agent":              "/srv/agent",
		"-srv-agent-repos-api":    "/srv/agent",
		"-srv-agent-b":            "/srv/agent-b", // not /srv/agent's, despite the prefix
		"-srv-agent-b-repos":      "/srv/agent-b",
		"-srv-agentx":             "",
		"-home-someone-elsewhere": "",
	}
	for project, want := range cases {
		if got := ClaudeProjectOwner(project, wd); got != want {
			t.Fatalf("%s: got %q want %q", project, got, want)
		}
	}
	if ClaudeSlug("/srv/a.b_c/d") != "-srv-a-b-c-d" {
		t.Fatalf("slug: %s", ClaudeSlug("/srv/a.b_c/d"))
	}
	if DirOwner("/srv/agent/repos/x", []string{"/srv/agent", "/srv/agent/repos"}) != "/srv/agent/repos" {
		t.Fatal("deepest workdir owns a nested path")
	}
	if DirOwner("/srv/agent-b/x", []string{"/srv/agent"}) != "" {
		t.Fatal("a sibling with a shared prefix is not inside")
	}
}

func TestCodexReachedMarkerBeatsCredits(t *testing.T) {
	// Credits carry an account past 100%, but not past an explicit limit.
	line := strings.Replace(strings.Replace(codexUsage(40, ""), `"has_credits":false`, `"has_credits":true`, 1),
		`"rate_limit_reached_type":null`, `"rate_limit_reached_type":"usage_limit_reached"`, 1)
	if s, _ := ParseCodexLine([]byte(line)); s.Kind != SigLimited {
		t.Fatalf("got %+v", s)
	}
}

func TestCodexOnlyTokenCountsCarryUsage(t *testing.T) {
	line := strings.Replace(codexUsage(100, ""), `"type":"token_count"`, `"type":"other_event"`, 1)
	if _, ok := ParseCodexLine([]byte(line)); ok {
		t.Fatal("only token_count records are read")
	}
}
