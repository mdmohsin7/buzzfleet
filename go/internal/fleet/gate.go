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

	holding  bool
	inflight map[string]bool // ids of forwarded prompts awaiting a response
	queue    []*Pending
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
		if g.holding {
			// Already this harness's turn: a second prompt from the same
			// harness (its own queue or steering) goes straight through.
			g.forward(m.ID, line)
			return
		}
		ch, rt := PromptAnchor(m.Params)
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
func (g *Gate) FromAgent(line []byte) {
	g.ToClient(line)
	m, ok := ParseACP(line)
	if !ok || !m.HasResult || m.ID == nil {
		return
	}
	if g.inflight[string(m.ID)] {
		delete(g.inflight, string(m.ID))
		if len(g.inflight) == 0 && g.holding {
			_ = g.Lock.Unlock()
			g.holding = false
			g.changed()
		}
	}
}

// Tick tries the lock for waiting prompts, announces long waits, and fails
// prompts that have waited beyond the limit.
func (g *Gate) Tick(now time.Time) {
	if len(g.queue) == 0 {
		return
	}
	if !g.holding && (g.Yield == nil || !g.Yield(g.queue[0].Since)) {
		got, err := g.Lock.TryLock()
		if err == nil && got {
			g.holding = true
			q := g.queue
			g.queue = nil
			for _, p := range q {
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
	Harness string `json:"harness"` // unit instance
	PID     int    `json:"pid"`
	Since   int64  `json:"since"`
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
	case waitingSince > 0 && holder != nil:
		return fmt.Sprintf("waiting on %s since %s", label(holder.Harness), clock(waitingSince))
	case waitingSince > 0:
		return fmt.Sprintf("waiting for its turn since %s", clock(waitingSince))
	case holder != nil && holder.Harness == self:
		return fmt.Sprintf("in a turn since %s", clock(holder.Since))
	}
	return ""
}
