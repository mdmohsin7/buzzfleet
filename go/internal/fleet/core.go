// Package fleet holds the decisions buzzfleet makes about your agents.
//
// Everything here is pure: no processes, no network, no clock. The logic that
// decides whether to restart something you depend on should be testable without
// a relay, a systemd session, or a wedged harness to hand.
package fleet

import (
	"fmt"
	"regexp"
	"strings"
)

// ── addressing ────────────────────────────────────────────────────────────────

// Config is the supervisor's identity and manners.
type Config struct {
	Name       string `json:"name"`       // this host's supervisor, e.g. "fleet-host-1"
	Broadcast  string `json:"broadcast"`  // shared name every host answers to, e.g. "fleet"
	Host       string `json:"host"`       // hostname used to label replies
	Relay      string `json:"relay"`      //
	Channel    string `json:"channel"`    //
	Owner      string `json:"owner"`      // only this pubkey is obeyed
	PrivateKey string `json:"privateKey"` //
	PublicKey  string `json:"publicKey"`  //

	HealthSeconds  int `json:"healthSeconds"`  // how often to assess agents
	HealAfterPolls int `json:"healAfterPolls"` // consecutive bad readings before acting
	MaxHealsPerHr  int `json:"maxHealsPerHour"`
	HealWindowSecs int `json:"healWindowSecs"`

	NoAutoHeal bool `json:"noAutoHeal"`
}

func (c *Config) WithDefaults() *Config {
	if c.Name == "" {
		c.Name = "fleet"
	}
	if c.HealthSeconds == 0 {
		c.HealthSeconds = 45
	}
	if c.HealAfterPolls == 0 {
		c.HealAfterPolls = 2
	}
	if c.MaxHealsPerHr == 0 {
		c.MaxHealsPerHr = 3
	}
	if c.HealWindowSecs == 0 {
		c.HealWindowSecs = 3600
	}
	return c
}

// Message is the part of a Nostr event this package cares about.
type Message struct {
	ID      string
	PubKey  string
	Content string
	PTags   []string // values of "p" tags
}

// Command is a message understood as an instruction to this supervisor.
type Command struct {
	Text        string
	AddressedTo string
}

// ParseCommand decides whether a message is for us, and strips the address off.
//
// Accepts "@fleet status", "fleet: status", "fleet, status", or a bare p-tag
// mention. Broadcast lets one channel reach every machine: each supervisor
// answers to its own name and to the shared word, so a fleet spanning hosts
// needs no central coordinator.
func ParseCommand(m Message, cfg *Config) (Command, bool) {
	content := strings.TrimSpace(m.Content)
	if content == "" {
		return Command{}, false
	}

	for _, n := range []string{cfg.Name, cfg.Broadcast} {
		if n == "" {
			continue
		}
		q := regexp.QuoteMeta(n)
		if re := regexp.MustCompile(`(?i)^@?` + q + `\s*[:,]?\s+`); re.MatchString(content) {
			return Command{Text: strings.TrimSpace(re.ReplaceAllString(content, "")), AddressedTo: n}, true
		}
		if regexp.MustCompile(`(?i)^@?` + q + `$`).MatchString(content) {
			return Command{Text: "help", AddressedTo: n}, true
		}
	}

	for _, p := range m.PTags {
		if p == cfg.PublicKey {
			stripped := regexp.MustCompile(`^@\S+\s+`).ReplaceAllString(content, "")
			return Command{Text: strings.TrimSpace(stripped), AddressedTo: cfg.Name}, true
		}
	}
	return Command{}, false
}

// ParseVerb splits a command into its verb and arguments.
func ParseVerb(text string) (string, []string) {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) == 0 {
		return "", nil
	}
	return strings.ToLower(parts[0]), parts[1:]
}

// ── authorisation and replay ──────────────────────────────────────────────────

// ShouldHandle gates every inbound message. Commands must execute at most once,
// ever: without the seen-set a supervisor restart re-reads history and re-runs
// everything it finds.
func ShouldHandle(m Message, seen map[string]bool, cfg *Config) bool {
	if seen[m.ID] {
		return false
	}
	if m.PubKey == cfg.PublicKey {
		return false // never answer ourselves
	}
	return m.PubKey == cfg.Owner // only the owner commands the fleet
}

// ── health ────────────────────────────────────────────────────────────────────

type State string

const (
	StateOK      State = "ok"
	StateWedged  State = "wedged"  // running, but the relay cannot see it
	StateStalled State = "stalled" // on the relay, but its agent process is gone
	StateDown    State = "down"    // unit is not running
	StateUnknown State = "unknown"
)

// Tri is a three-valued answer: the difference between "no" and "could not tell"
// decides whether an agent gets restarted, so it must not collapse to a bool.
type Tri int

const (
	TriUnknown Tri = iota
	TriYes
	TriNo
)

type Assessment struct {
	Name   string
	Unit   string
	State  State
	Detail string
}

// Assess turns raw observations into one of the states above.
func Assess(name, unit string, present, childAlive Tri, hasIdentity bool) Assessment {
	a := Assessment{Name: name, Unit: unit}
	switch {
	case unit != "active":
		a.State, a.Detail = StateDown, "unit "+unit
	case !hasIdentity:
		a.State, a.Detail = StateUnknown, "no identity on this host"
	case present == TriUnknown:
		// Our own connectivity failure. Never blamed on the agent, never healed.
		a.State, a.Detail = StateUnknown, "relay unreachable"
	case present == TriNo:
		a.State, a.Detail = StateWedged, "running but absent from relay"
	case childAlive == TriNo:
		a.State, a.Detail = StateStalled, "present on relay but its agent process is gone"
	default:
		a.State, a.Detail = StateOK, "present on relay"
	}
	return a
}

func (a Assessment) NeedsHeal() bool {
	return a.State == StateWedged || a.State == StateStalled
}

// ── reply placement ───────────────────────────────────────────────────────────

// ThreadLongerThan is the point past which a reply is threaded rather than
// posted inline. Short answers belong in the channel: you asked for `status`
// because you wanted to see it now, and on a phone a thread is an extra tap.
// Long ones — a screen of journal output — would flood the channel instead.
const ThreadLongerThan = 12

// ShouldThread reports whether a reply is long enough to belong in a thread.
func ShouldThread(reply string) bool {
	if strings.Count(reply, "\n")+1 > ThreadLongerThan {
		return true
	}
	return len(reply) > 900
}

// ── circuit breaker ───────────────────────────────────────────────────────────

type Action string

const (
	ActionNone   Action = "none"
	ActionClear  Action = "clear"
	ActionStrike Action = "strike"
	ActionHeal   Action = "heal"
	ActionGiveUp Action = "give-up"
)

type Decision struct {
	Action  Action
	Strikes int
	Reason  string
}

// HealsInWindow counts recent restarts of one agent.
func HealsInWindow(history map[string][]int64, name string, now int64, window int64) int {
	n := 0
	for _, t := range history[name] {
		if now-t < window {
			n++
		}
	}
	return n
}

// PruneHeals drops restarts that have aged out, so state cannot grow forever.
func PruneHeals(history map[string][]int64, now int64, window int64) {
	for name, list := range history {
		kept := list[:0]
		for _, t := range list {
			if now-t < window {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(history, name)
		} else {
			history[name] = kept
		}
	}
}

// Decide works out what to do about one agent this tick.
//
// Restarting a wedged agent fixes a transient fault. Restarting one that is
// broken — bad config, deleted workdir, revoked key — just does it forever,
// every couple of minutes, for as long as nobody is watching. So heals are
// counted, and after a few the supervisor stops and says so once.
func Decide(name string, a Assessment, strikes int, history map[string][]int64, now int64, cfg *Config) Decision {
	if !a.NeedsHeal() {
		if strikes > 0 {
			return Decision{Action: ActionClear}
		}
		return Decision{Action: ActionNone}
	}

	n := strikes + 1
	if n < cfg.HealAfterPolls {
		return Decision{Action: ActionStrike, Strikes: n}
	}

	if recent := HealsInWindow(history, name, now, int64(cfg.HealWindowSecs)); recent >= cfg.MaxHealsPerHr {
		return Decision{
			Action:  ActionGiveUp,
			Strikes: n,
			Reason: fmt.Sprintf(
				"%s has been restarted %d times in the last %d minutes and is %s again. "+
					"Not restarting it any more — something is broken that a restart does not fix. "+
					"Check `logs %s`, then `restart %s` once you have fixed it.",
				name, recent, cfg.HealWindowSecs/60, a.State, name, name),
		}
	}
	return Decision{Action: ActionHeal}
}
