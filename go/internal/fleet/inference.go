package fleet

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ── inference health ──────────────────────────────────────────────────────────
//
// An agent can be running, present on the relay, with its coding agent alive,
// and still fail every turn: its OAuth session expired, or the account hit its
// weekly limit. Nothing above notices, because nothing above is broken — the
// harness answers each mention with the model's error and goes back to waiting.
//
// The evidence is in the coding agent's own transcript, as a structured record.
// A restart cannot fix either condition, so these states are reported, never
// healed.

const (
	StateAuth    State = "auth"    // the model provider refused the agent's credentials
	StateLimited State = "limited" // the account is out of usage until a reset
)

type SignalKind int

const (
	SigNone SignalKind = iota
	SigOK              // a turn completed normally
	SigAuth
	SigLimited
)

// Signal is one turn outcome found in a transcript.
type Signal struct {
	At     time.Time
	Kind   SignalKind
	Text   string    // the provider's message, for the channel
	Resets time.Time // when a limit lifts, if the message said
}

var (
	limitTextRe = regexp.MustCompile(`(?i)hit your [a-z ]*limit|usage limit|spend limit|rate.?limit`)
)

// claudeRecord is the part of a Claude Code transcript line that matters.
type claudeRecord struct {
	Type              string `json:"type"`
	Timestamp         string `json:"timestamp"`
	IsSidechain       bool   `json:"isSidechain"`
	IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
	Error             string `json:"error"`
	Message           struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// ParseClaudeLine reads one line of a Claude Code session transcript.
//
// Failed turns are written as an assistant record with isApiErrorMessage set
// and the API's error class in "error": rate_limit for every kind of usage
// limit, authentication_failed for an expired or revoked login. Anything else
// assistant-shaped is a turn that reached the model.
func ParseClaudeLine(line []byte) (Signal, bool) {
	var r claudeRecord
	if json.Unmarshal(line, &r) != nil || r.Type != "assistant" || r.IsSidechain {
		return Signal{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil {
		return Signal{}, false
	}
	if !r.IsAPIErrorMessage {
		return Signal{At: at, Kind: SigOK}, true
	}
	text := contentText(r.Message.Content)
	s := Signal{At: at, Text: text}
	switch {
	case r.Error == "authentication_failed":
		s.Kind = SigAuth
	case r.Error == "rate_limit" || r.Error == "billing_error":
		s.Kind = SigLimited
	case limitTextRe.MatchString(text):
		// A limit reported through another error, e.g. compaction failing
		// because the account is out of usage.
		s.Kind = SigLimited
	default:
		// Overloaded, prompt too long, a server error: a failed turn, but not
		// something wrong with the account. Neither healthy nor broken.
		return Signal{}, false
	}
	if s.Kind == SigLimited {
		s.Resets, _ = ParseReset(text, at)
	}
	return s, true
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// codexRecord is the part of a Codex rollout line that matters.
type codexRecord struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Primary              *codexWindow `json:"primary"`
			Secondary            *codexWindow `json:"secondary"`
			RateLimitReachedType *string      `json:"rate_limit_reached_type"`
			Credits              *struct {
				HasCredits bool `json:"has_credits"`
				Unlimited  bool `json:"unlimited"`
			} `json:"credits"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"` // unix seconds
}

// ParseCodexLine reads one line of a Codex session rollout.
//
// Codex does not write its error events to the rollout, and it records every
// turn as task_complete, failed or not, so neither says whether a turn worked.
// What it does persist is a usage snapshot with each token count: how much of
// each rate-limit window is used and when that window resets. A window at 100%,
// or a reached-limit marker, means the account is out, until the reset. Any
// other snapshot means the model just answered. Codex auth failures leave no
// record at all, so they cannot be detected here.
//
// Tool output in the same file is full of "401 Unauthorized" from commands the
// agent ran; only these structured records are read.
func ParseCodexLine(line []byte) (Signal, bool) {
	var r codexRecord
	if json.Unmarshal(line, &r) != nil || r.Type != "event_msg" || r.Payload.Type != "token_count" || r.Payload.RateLimits == nil {
		return Signal{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil {
		return Signal{}, false
	}
	rl := r.Payload.RateLimits
	var resets int64
	out := rl.RateLimitReachedType != nil && *rl.RateLimitReachedType != ""
	for _, w := range []*codexWindow{rl.Primary, rl.Secondary} {
		if w != nil && w.UsedPercent >= 100 {
			out = true
			resets = max(resets, w.ResetsAt)
		}
	}
	// Past 100% an account with credits carries on, paying for it.
	if out && rl.Credits != nil && (rl.Credits.HasCredits || rl.Credits.Unlimited) && (rl.RateLimitReachedType == nil || *rl.RateLimitReachedType == "") {
		out = false
	}
	if !out {
		return Signal{At: at, Kind: SigOK}, true
	}
	s := Signal{At: at, Kind: SigLimited, Text: "Codex usage limit reached"}
	if rl.RateLimitReachedType != nil && *rl.RateLimitReachedType != "" {
		s.Text = "Codex usage limit reached (" + *rl.RateLimitReachedType + ")"
	}
	if resets > 0 {
		s.Resets = time.Unix(resets, 0).UTC()
	}
	return s, true
}

var (
	// "resets Oct 30, 10pm (UTC)", "resets 7:10pm (UTC)", "resets 10pm (UTC)"
	resetRe = regexp.MustCompile(`(?i)resets\s+(?:([A-Z][a-z]{2})\s+(\d{1,2}),\s*)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)\s*\(UTC\)`)
	// "try again in 2 days 3 hours 5 minutes"
	againInRe = regexp.MustCompile(`(?i)try again in\s+((?:\d+\s*(?:days?|hours?|minutes?|mins?)\s*)+)`)
	unitRe    = regexp.MustCompile(`(?i)(\d+)\s*(day|hour|min)`)
)

// ParseReset finds when a limit lifts, from the provider's message. The time
// of day alone means its next occurrence after the error.
func ParseReset(text string, at time.Time) (time.Time, bool) {
	at = at.UTC()
	if m := resetRe.FindStringSubmatch(text); m != nil {
		hour := atoi(m[3]) % 12
		if strings.EqualFold(m[5], "pm") {
			hour += 12
		}
		min := atoi(m[4])
		if m[1] != "" {
			mon, err := time.Parse("Jan", m[1])
			if err != nil {
				return time.Time{}, false
			}
			t := time.Date(at.Year(), mon.Month(), atoi(m[2]), hour, min, 0, 0, time.UTC)
			if t.Before(at.Add(-24 * time.Hour)) { // "Jan 2" written in late December
				t = t.AddDate(1, 0, 0)
			}
			return t, true
		}
		t := time.Date(at.Year(), at.Month(), at.Day(), hour, min, 0, 0, time.UTC)
		if !t.After(at) {
			t = t.Add(24 * time.Hour)
		}
		return t, true
	}
	if m := againInRe.FindStringSubmatch(text); m != nil {
		var d time.Duration
		for _, u := range unitRe.FindAllStringSubmatch(m[1], -1) {
			n := time.Duration(atoi(u[1]))
			switch strings.ToLower(u[2]) {
			case "day":
				d += n * 24 * time.Hour
			case "hour":
				d += n * time.Hour
			default:
				d += n * time.Minute
			}
		}
		if d > 0 {
			return at.Add(d), true
		}
	}
	return time.Time{}, false
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Latest is the most recent signal: what the agent's last turn says about it.
func Latest(sigs []Signal) Signal {
	var l Signal
	for _, s := range sigs {
		if s.Kind != SigNone && (l.Kind == SigNone || s.At.After(l.At)) {
			l = s
		}
	}
	return l
}

// InferenceState turns an agent's latest signal into a state. A limit with a
// known reset is assumed lifted once the reset has passed; if it has not, the
// next turn says so and the state comes straight back.
func InferenceState(s Signal, now time.Time) (State, string) {
	switch s.Kind {
	case SigAuth:
		return StateAuth, "model login failed: " + firstLine(s.Text)
	case SigLimited:
		if !s.Resets.IsZero() && now.After(s.Resets) {
			// Assumed, not shown: no turn has run since the reset.
			return StateOK, "limit should have reset at " + s.Resets.UTC().Format("Jan 2 15:04 UTC")
		}
		d := "usage limit: " + firstLine(s.Text)
		if !s.Resets.IsZero() {
			d = fmt.Sprintf("usage limit until %s", s.Resets.UTC().Format("Jan 2 15:04 UTC"))
		}
		return StateLimited, d
	}
	return StateOK, ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	// Provider messages separate clauses with " · "; the first is the gist.
	s, _, _ = strings.Cut(s, " · ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// WithInference overlays the inference state on a relay assessment. Relay
// faults come first: a wedged agent is restarted whatever its account is doing,
// because a restart fixes the wedge. A healthy-looking agent whose turns are
// all failing is what this exists to expose.
func WithInference(a Assessment, st State, detail string) Assessment {
	if a.State != StateOK {
		return a
	}
	switch {
	case st == StateAuth || st == StateLimited:
		a.State, a.Detail = st, detail
	case detail != "":
		a.Detail += "; " + detail
		a.LimitAssumedLifted = true
	}
	return a
}

// ── telling people, once ──────────────────────────────────────────────────────

// InferenceNotices compares last tick's inference states with this tick's and
// returns what to post. One account limit takes down every agent on that
// account at once, so agents that entered the same state for the same reason
// are named in one message, not one each.
func InferenceNotices(host string, prev map[string]string, now []Assessment) (notices []string, next map[string]string) {
	next = map[string]string{}
	groups := map[string][]string{}
	var order []string
	var recovered, reset []string
	for _, a := range now {
		cur := ""
		if a.State == StateAuth || a.State == StateLimited {
			cur = string(a.State) + "|" + a.Detail
			next[a.Name] = cur
		}
		was, had := prev[a.Name]
		switch {
		case cur != "" && cur != was:
			if _, ok := groups[cur]; !ok {
				order = append(order, cur)
			}
			groups[cur] = append(groups[cur], a.Name)
		case cur == "" && had:
			// Only a real recovery: an agent that went down or vanished has
			// not been shown to work again.
			if a.State == StateOK && a.LimitAssumedLifted {
				reset = append(reset, a.Name)
			} else if a.State == StateOK {
				recovered = append(recovered, a.Name)
			} else {
				next[a.Name] = was
			}
		}
	}
	for _, k := range order {
		st, detail, _ := strings.Cut(k, "|")
		names := groups[k]
		plural := len(names) > 1
		var what, fix string
		if State(st) == StateLimited {
			what = pick(plural, "are out of model usage", "is out of model usage")
			fix = "It clears by itself when the limit resets, or on the next good turn."
		} else {
			what = pick(plural, "cannot log in to their model", "cannot log in to its model")
			fix = "Someone has to log in again on the host; it clears on the next good turn."
		}
		notices = append(notices, fmt.Sprintf(
			"**%s** — %s %s (%s). Every turn is failing, so I am not restarting %s: a restart does not fix this. %s",
			host, boldList(names), what, detail, pick(plural, "them", "it"), fix))
	}
	if len(reset) > 0 {
		// Nothing has run since, so this is a forecast, not a recovery.
		notices = append(notices, fmt.Sprintf("**%s** — %s %s past %s usage reset, so %s should work again. If not, the next turn will say so.",
			host, boldList(reset), pick(len(reset) > 1, "are", "is"), pick(len(reset) > 1, "their", "its"), pick(len(reset) > 1, "they", "it")))
	}
	if len(recovered) > 0 {
		notices = append(notices, fmt.Sprintf("**%s** — %s %s answering normally again.",
			host, boldList(recovered), pick(len(recovered) > 1, "are", "is")))
	}
	return notices, next
}

func pick(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func boldList(names []string) string { return "**" + strings.Join(names, "**, **") + "**" }

// ── finding an agent's transcripts ────────────────────────────────────────────

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// ClaudeSlug is the directory name Claude Code keeps a working directory's
// sessions under: every character that is not a letter or digit becomes "-".
func ClaudeSlug(dir string) string {
	return nonAlnum.ReplaceAllString(filepath.Clean(dir), "-")
}

// ClaudeProjectOwner says which workdir a Claude project directory belongs to:
// the workdir itself, or a directory below it. The slug loses the difference
// between "/" and "-", so /srv/agent and /srv/agent-b both prefix-match
// "-srv-agent-b-repo"; the longest match is the one that owns it. The loss
// cannot be undone: /srv/agent/b and /srv/agent-b produce the same slug, so a
// repo at /srv/agent/b is credited to an agent whose workdir is /srv/agent-b.
func ClaudeProjectOwner(project string, workdirs []string) string {
	best, bestLen := "", -1
	for _, w := range workdirs {
		s := ClaudeSlug(w)
		if (project == s || strings.HasPrefix(project, s+"-")) && len(s) > bestLen {
			best, bestLen = w, len(s)
		}
	}
	return best
}

// UnderDir reports whether path is dir or inside it.
func UnderDir(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// DirOwner is the workdir that contains path, preferring the deepest when one
// agent's workdir sits inside another's.
func DirOwner(path string, workdirs []string) string {
	best := ""
	for _, w := range workdirs {
		if UnderDir(path, w) && len(filepath.Clean(w)) > len(best) {
			best = filepath.Clean(w)
		}
	}
	return best
}
