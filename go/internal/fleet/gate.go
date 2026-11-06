package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// ── turn gate ─────────────────────────────────────────────────────────────────
//
// An agent in several communities runs one harness per community, all in the
// same workdir. Two turns at once would edit the same files, the same git
// checkouts and the coding agent's own memory at the same time. Today an agent
// never runs two turns at once — buzz-acp drives one coding-agent process and
// queues events while it works — and the gate keeps it that way across
// communities.
//
// It sits between buzz-acp and the coding agent, passing ACP JSON-RPC through
// untouched, except that a session/prompt waits for the agent's turn lock
// before it is forwarded. Within one harness the lock is re-entrant: buzz-acp's
// own queueing and steering never wait on themselves. See
// docs/design/multi-community.md, section 3.
//
// This is the decision logic only. The lock, the clock, the pipes and the
// queued notice are supplied by the caller, so every rule here is testable.

// QueuedAfter is how long a prompt waits before the people who sent it are
// told it is queued.
const QueuedAfter = 60 * time.Second

// QuietAfter is how long a coding agent must stay silent before work it did
// on its own, outside any prompt, is taken to be over.
const QuietAfter = 30 * time.Second

// BackgroundCap bounds how long a turn that left background work running keeps
// the agent's turn after its prompt ended: a background server that never
// exits must not hold every other community off forever.
const BackgroundCap = 30 * time.Minute

// QueuedNotice is what they are told. It says the agent is alive and busy, not
// where: the other community's business is its own.
const QueuedNotice = "Busy with another task right now. This is queued, and I'll get to it as soon as that finishes."

// ACPMessage is the part of a JSON-RPC line the gate looks at.
type ACPMessage struct {
	ID        json.RawMessage `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	HasResult bool            `json:"-"`
}

// ParseACP reads one line. A line that is not JSON is passed through as is;
// the gate never drops or rewrites what it does not understand.
func ParseACP(line []byte) (ACPMessage, bool) {
	var raw struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &raw) != nil {
		return ACPMessage{}, false
	}
	m := ACPMessage{ID: raw.ID, Method: raw.Method, Params: raw.Params}
	m.HasResult = raw.Method == "" && (raw.Result != nil || raw.Error != nil)
	if string(m.ID) == "null" {
		m.ID = nil
	}
	return m, true
}

func sessionID(params json.RawMessage) string {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	return p.SessionID
}

var (
	// Only buzz-acp's own fixed lines count: channel names and message
	// content are written by people, and agents quote buzz CLI syntax all
	// the time. The channel line is "Channel: <name> (#<uuid>)", so the uuid
	// is the last "(#…)" on its line; the reply line is one of buzz-acp's
	// two exact reply instructions, appended after the content.
	promptChannelRe = regexp.MustCompile(`(?m)^Channel: .*\(#([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\)$`)
	promptReplyRe   = regexp.MustCompile("IMPORTANT: (?:This is a new top-level message\\. )?For ordinary replies in this turn, use `--reply-to ([0-9a-f]{64})` on `buzz messages send`")
)

// PromptAnchor finds where a prompt came from, so the queued notice can land
// next to it: buzz-acp writes the channel as "Channel: <name> (#<uuid>)" and
// the reply anchor as "use `--reply-to <event id>`". Either may be absent, and
// a notice that cannot be placed is simply not posted.
func PromptAnchor(params json.RawMessage) (channel, replyTo string) {
	var p struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	for _, block := range p.Prompt {
		if channel == "" {
			if m := promptChannelRe.FindStringSubmatch(block.Text); m != nil {
				channel = m[1]
			}
		}
		// buzz-acp appends its reply instruction after the event content,
		// so the last match is its own, even when a message quotes it.
		if m := promptReplyRe.FindAllStringSubmatch(block.Text, -1); len(m) > 0 {
			replyTo = m[len(m)-1][1]
		}
	}
	return channel, replyTo
}

// Locker is the agent's turn lock, as the gate needs it.
type Locker interface {
	TryLock() (bool, error) // non-blocking
	Unlock() error
}

// Pending is a prompt waiting for the lock.
type Pending struct {
	ID       json.RawMessage
	Session  string
	Line     []byte
	Since    time.Time
	Channel  string
	ReplyTo  string
	notified bool
}

// Gate is the state of one harness's gate.
type Gate struct {
	Lock     Locker
	ToAgent  func([]byte)  // write a line to the coding agent
	ToClient func([]byte)  // write a line to buzz-acp
	Queued   func(Pending) // tell the sender a prompt is queued
	MaxWait  time.Duration // a prompt waiting longer than this fails
	Changed  func(holding bool, waiting []Pending)
	// Yield reports whether another harness of the agent has been waiting
	// longer than since. The lock goes to whoever asks first once it is free,
	// and a harness whose community keeps it busy would otherwise win every
	// time; yielding to an older waiter makes turns first come, first served.
	Yield func(since time.Time) bool
	// Logf, if set, notes each turn's start and end and each wait, which is
	// what shows whether turns really were one at a time.
	Logf func(format string, a ...any)

	holding  bool
	inflight map[string]bool // ids of forwarded prompts awaiting a response
	queue    []*Pending

	// A coding agent can work outside any prompt. Claude Code runs a long
	// command in the background, ends the prompt, and later wakes itself to
	// carry on, with no prompt from buzz-acp. A turn that started background
	// work therefore keeps the agent's turn after its prompt ends, until
	// that self-started work has gone quiet (or the cap passes); and work
	// the agent starts on its own while nobody holds the turn takes it.
	//
	// Neither hold may keep the turn from the other communities for good:
	// while one is on, this harness's own new prompts queue and yield like
	// anyone else's, and both are capped from the moment the hold began.
	bgCalls    map[string]bool // tool calls of this hold that started background work
	afterglow  bool            // holding past the prompts, for background work
	woke       bool            // the agent has worked on its own since
	autonomous bool            // holding for work the agent started on its own
	holdStart  time.Time       // when the afterglow or autonomous hold began; never reset
	lastActive time.Time
	overlapped bool // warned already about this burst of overlapping work
}

// Background reports whether the turn is held for work outside a prompt.
func (g *Gate) Background() bool { return g.afterglow || g.autonomous }

func (g *Gate) logf(format string, a ...any) {
	if g.Logf != nil {
		g.Logf(format, a...)
	}
}

func (g *Gate) changed() {
	if g.Changed == nil {
		return
	}
	var w []Pending
	for _, p := range g.queue {
		w = append(w, *p)
	}
	g.Changed(g.holding, w)
}

// FromClient handles one line from buzz-acp.
func (g *Gate) FromClient(line []byte, now time.Time) {
	m, ok := ParseACP(line)
	if !ok {
		g.ToAgent(line)
		return
	}
	switch {
	case m.Method == "session/prompt" && m.ID != nil:
		if g.holding && len(g.inflight) > 0 {
			// Mid-prompt: a second prompt from the same harness (its own
			// queue or steering) goes straight through.
			g.forward(m.ID, line)
			return
		}
		// Otherwise it queues, even while this harness holds the turn for
		// background work: a hold for work outside a prompt must not let
		// its own community jump ahead of another that has been waiting.
		ch, rt := PromptAnchor(m.Params)
		g.logf("prompt %s arrived; waiting for the agent's turn", m.ID)
		g.queue = append(g.queue, &Pending{ID: m.ID, Session: sessionID(m.Params), Line: line, Since: now, Channel: ch, ReplyTo: rt})
		g.Tick(now) // the lock may well be free
		if !g.holding {
			g.changed()
		}
	case m.Method == "session/cancel":
		// A cancel is a notification, so it cannot be answered. A prompt for
		// that session still waiting for the lock is answered instead, as
		// cancelled, and never reaches the agent. The cancel itself goes on,
		// for whatever the agent is doing.
		sid := sessionID(m.Params)
		kept := g.queue[:0]
		for _, p := range g.queue {
			if p.Session == sid {
				g.ToClient(response(p.ID, `{"stopReason":"cancelled"}`))
				continue
			}
			kept = append(kept, p)
		}
		changed := len(kept) != len(g.queue)
		g.queue = kept
		g.ToAgent(line)
		if changed {
			g.changed()
		}
	default:
		g.ToAgent(line)
	}
}

// FromAgent handles one line from the coding agent. Everything is passed on —
// agent-to-client requests such as file access and permission prompts must
// flow while a turn holds the lock — and a prompt's response ends its turn.
func (g *Gate) FromAgent(line []byte, now time.Time) {
	g.ToClient(line)
	m, ok := ParseACP(line)
	if !ok {
		return
	}
	if m.Method == "session/update" {
		g.agentActive(m.Params, now)
		return
	}
	if !m.HasResult || m.ID == nil || !g.inflight[string(m.ID)] {
		return
	}
	delete(g.inflight, string(m.ID))
	if len(g.inflight) > 0 || !g.holding {
		return
	}
	switch {
	case g.afterglow || g.autonomous:
		// A prompt inside an existing hold: the hold goes on as it was,
		// with its cap still counting from when it began.
	case len(g.bgCalls) > 0:
		// The prompt is over but its background work is not: keep the
		// turn until the agent, woken by that work, has finished.
		g.afterglow, g.holdStart, g.woke, g.lastActive = true, now, false, now
		g.logf("prompt %s done, but it left background work running; holding the turn until that is finished", m.ID)
		g.changed()
	default:
		g.release("prompt " + string(m.ID) + " done; turn released")
	}
}

func (g *Gate) release(why string) {
	_ = g.Lock.Unlock()
	g.holding, g.afterglow, g.autonomous, g.woke = false, false, false, false
	g.bgCalls = nil
	g.logf("%s", why)
	g.changed()
}

// agentActive notes output from the coding agent: whether it started
// background work, and whether it is working outside any prompt.
func (g *Gate) agentActive(params json.RawMessage, now time.Time) {
	var p struct {
		Update struct {
			Kind     string `json:"sessionUpdate"`
			CallID   string `json:"toolCallId"`
			RawInput struct {
				Background bool `json:"run_in_background"`
			} `json:"rawInput"`
		} `json:"update"`
	}
	_ = json.Unmarshal(params, &p)
	k := p.Update.Kind
	if (k == "tool_call" || k == "tool_call_update") && p.Update.RawInput.Background && g.holding {
		if g.bgCalls == nil {
			g.bgCalls = map[string]bool{}
		}
		g.bgCalls[p.Update.CallID] = true
	}
	// Updates about a call that went to the background are its launch, not
	// the agent waking: Claude Code marks such a call completed the moment
	// it is backgrounded, long before the command itself ends.
	if g.bgCalls[p.Update.CallID] && p.Update.CallID != "" {
		return
	}
	// Only real work counts as activity: the agent announcing its commands,
	// mode or usage is not doing anything.
	switch k {
	case "available_commands_update", "current_mode_update", "usage_update", "session_info_update", "config_option_update", "":
		return
	}
	if len(g.inflight) > 0 {
		return // inside a prompt: the ordinary case
	}
	g.lastActive = now
	switch {
	case g.afterglow:
		g.woke = true
	case g.autonomous:
	case g.holding:
	default:
		// Working on its own with nobody holding its turn: take it, so no
		// other community starts a turn on top of this one — unless another
		// community is already waiting for the turn, which comes first.
		waiting := g.Yield != nil && g.Yield(now)
		if waiting {
			// fall through to the overlap warning
		} else if got, err := g.Lock.TryLock(); err == nil && got {
			g.holding, g.autonomous, g.overlapped, g.holdStart = true, true, false, now
			g.logf("the agent started working on its own; holding its turn while it does")
			g.changed()
			return
		}
		if !g.overlapped {
			g.overlapped = true
			g.logf("WARNING: the agent started working on its own while another of its communities holds the turn; their work may overlap")
		}
	}
}

// Tick tries the lock for waiting prompts, announces long waits, and fails
// prompts that have waited beyond the limit.
func (g *Gate) Tick(now time.Time) {
	idle := len(g.inflight) == 0
	switch {
	case (g.afterglow || g.autonomous) && now.Sub(g.holdStart) >= BackgroundCap:
		// Checked first and regardless of prompts: the cap is what makes
		// sure no hold keeps the other communities off for good.
		g.inflight = nil
		g.release(fmt.Sprintf("work outside a prompt still going after %s; turn released anyway, so other communities are not held off", BackgroundCap))
	case g.afterglow && idle && g.woke && now.Sub(g.lastActive) >= QuietAfter:
		g.release("the background work and what the agent did after it are finished; turn released")
	case g.autonomous && idle && now.Sub(g.lastActive) >= QuietAfter:
		g.release("the agent's own work is finished; turn released")
	case !g.holding && g.overlapped && now.Sub(g.lastActive) >= QuietAfter:
		g.overlapped = false
	}
	if len(g.queue) == 0 {
		return
	}
	// This harness already holds the turn for work outside a prompt: its own
	// new prompts may go on in that hold, but only if nobody else has been
	// waiting longer; otherwise they wait for the hold to end, and the older
	// waiter goes first.
	if g.holding && idle && (g.afterglow || g.autonomous) && (g.Yield == nil || !g.Yield(g.queue[0].Since)) {
		q := g.queue
		g.queue = nil
		for _, p := range q {
			g.logf("prompt %s started inside the agent's own work, after waiting %s", p.ID, now.Sub(p.Since).Round(time.Second))
			g.forward(p.ID, p.Line)
		}
		g.changed()
		return
	}
	if !g.holding && (g.Yield == nil || !g.Yield(g.queue[0].Since)) {
		got, err := g.Lock.TryLock()
		if err == nil && got {
			g.holding = true
			q := g.queue
			g.queue = nil
			for _, p := range q {
				g.logf("prompt %s started after waiting %s", p.ID, now.Sub(p.Since).Round(time.Second))
				g.forward(p.ID, p.Line)
			}
			g.changed()
			return
		}
	}
	kept := g.queue[:0]
	for _, p := range g.queue {
		waited := now.Sub(p.Since)
		if g.MaxWait > 0 && waited > g.MaxWait {
			g.ToClient(errorResponse(p.ID, fmt.Sprintf(
				"buzzfleet: this agent was busy in another community for over %s; the prompt was not started", g.MaxWait.Round(time.Minute))))
			continue
		}
		if !p.notified && waited >= QueuedAfter {
			p.notified = true
			if g.Queued != nil {
				g.Queued(*p)
			}
		}
		kept = append(kept, p)
	}
	if len(kept) != len(g.queue) {
		g.queue = kept
		g.changed()
	} else {
		g.queue = kept
	}
}

func (g *Gate) forward(id json.RawMessage, line []byte) {
	if g.inflight == nil {
		g.inflight = map[string]bool{}
	}
	g.inflight[string(id)] = true
	g.ToAgent(line)
}

// Holding reports whether this harness holds the agent's turn.
func (g *Gate) Holding() bool { return g.holding }

func response(id json.RawMessage, result string) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result))
}

func errorResponse(id json.RawMessage, msg string) []byte {
	b, _ := json.Marshal(msg)
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":%s}}`, id, b))
}

// ── who holds the turn ────────────────────────────────────────────────────────

// Holder is written into the lock file by the harness holding the turn, and a
// waiter's own record into its wait file, so status can say who is waiting on
// whom instead of a waiting harness looking stalled.
type Holder struct {
	Harness    string `json:"harness"` // unit instance
	PID        int    `json:"pid"`
	Since      int64  `json:"since"`
	Background bool   `json:"background,omitempty"` // held for work outside a prompt
}

// Waiter is another harness's record that it is waiting for the turn.
type Waiter struct {
	Since int64
	Alive bool // its gate is still running; a dead one's record is ignored
}

// OlderWaiter reports whether any live waiter has waited longer than since.
// Ties go to nobody yielding, so two waiters cannot both stand aside forever.
func OlderWaiter(since time.Time, others []Waiter) bool {
	for _, w := range others {
		if w.Alive && w.Since < since.Unix() {
			return true
		}
	}
	return false
}

// GateNote describes a harness's place in its agent's turn queue, for status.
func GateNote(self string, holder *Holder, waitingSince int64, now time.Time) string {
	label := func(inst string) string { return ParseInstance(inst).Label(true) }
	clock := func(t int64) string { return time.Unix(t, 0).UTC().Format("15:04 UTC") }
	switch {
	case waitingSince > 0 && holder != nil && holder.Background:
		return fmt.Sprintf("waiting on %s (background work) since %s", label(holder.Harness), clock(waitingSince))
	case waitingSince > 0 && holder != nil:
		return fmt.Sprintf("waiting on %s since %s", label(holder.Harness), clock(waitingSince))
	case holder != nil && holder.Harness == self && holder.Background:
		return fmt.Sprintf("working outside a prompt since %s", clock(holder.Since))
	case waitingSince > 0:
		return fmt.Sprintf("waiting for its turn since %s", clock(waitingSince))
	case holder != nil && holder.Harness == self:
		return fmt.Sprintf("in a turn since %s", clock(holder.Since))
	}
	return ""
}
