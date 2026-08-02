package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
	"github.com/nbd-wtf/go-nostr"
)

// The supervisor runs beside the agents rather than inside them.
//
// Buzz's own !shutdown / !cancel / !rotate live inside each harness, so they
// only ever reach a healthy agent — and an agent that has stopped answering
// cannot report that it has stopped answering. Sitting in the channel as its
// own member means fleet control works from every Buzz client today, and that
// a dead agent is still something someone can ask about.

// How long to let the relay's AUTH challenge arrive before signing it.
const authSettleDelay = 750 * time.Millisecond

type daemonState struct {
	Introduced  bool               `json:"introduced"`
	Seen        []string           `json:"seen"`
	HealHistory map[string][]int64 `json:"healHistory"`
	GaveUp      map[string]int64   `json:"gaveUp"`
}

func statePath(h *host.Host) string {
	if v := os.Getenv("BUZZFLEETD_STATE"); v != "" {
		return v
	}
	return filepath.Join(h.FleetHome, ".fleetd-state.json")
}

func loadState(h *host.Host) *daemonState {
	s := &daemonState{HealHistory: map[string][]int64{}, GaveUp: map[string]int64{}}
	b, err := os.ReadFile(statePath(h))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, s)
	if s.HealHistory == nil {
		s.HealHistory = map[string][]int64{}
	}
	if s.GaveUp == nil {
		s.GaveUp = map[string]int64{}
	}
	return s
}

func (s *daemonState) save(h *host.Host) {
	// Bounded: `since` on the subscription does the heavy lifting, this only
	// guards the same-second boundary.
	if len(s.Seen) > 200 {
		s.Seen = s.Seen[len(s.Seen)-200:]
	}
	b, _ := json.Marshal(s)
	_ = os.WriteFile(statePath(h), b, 0o600)
}

func logf(format string, a ...any) {
	fmt.Printf("%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

func runDaemon(h *host.Host, cfg *fleet.Config) {
	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}
	logf("buzzfleetd starting: host=%s relay=%s channel=%s name=%s", cfg.Host, cfg.Relay, cfg.Channel, cfg.Name)

	if _, err := h.Buzz(cfg.PrivateKey, cfg.Relay, "users", "set-profile",
		"--name", cfg.Name, "--about", "Fleet supervisor on "+cfg.Host); err != nil {
		logf("could not publish profile: %v", err)
	}

	state := loadState(h)
	seen := map[string]bool{}
	for _, id := range state.Seen {
		seen[id] = true
	}

	// Introduce itself once, so the command list is discoverable without anyone
	// having to be told that `help` exists. Once only — a supervisor that
	// reposts its manual on every restart becomes noise you learn to skip.
	if !state.Introduced {
		say(h, cfg, helpText(cfg), "")
		state.Introduced = true
		state.save(h)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go healthLoop(ctx, h, cfg, state)
	go subscribeLoop(ctx, h, cfg, state, seen)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logf("shutting down")
	cancel()
	state.save(h)
}

// ── relay subscription ────────────────────────────────────────────────────────

// Subscribing rather than polling: replies are immediate, and the supervisor
// does not spend the relay's rate-limit budget, whose exhaustion is the very
// failure it exists to detect.
func subscribeLoop(ctx context.Context, h *host.Host, cfg *fleet.Config, state *daemonState, seen map[string]bool) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := subscribeOnce(ctx, h, cfg, state, seen); err != nil {
			logf("relay: %v; reconnecting in %s", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func subscribeOnce(ctx context.Context, h *host.Host, cfg *fleet.Config, state *daemonState, seen map[string]bool) error {
	relay, err := nostr.RelayConnect(ctx, host.WSRelay(cfg.Relay))
	if err != nil {
		return fmt.Errorf("connect failed: %w", err)
	}
	defer relay.Close()
	logf("relay: connected")

	// NIP-42. Buzz requires it before a subscription is accepted, and go-nostr
	// waits for the relay's OK — subscribing before that ack races the relay's
	// own validation and is refused.
	//
	// The challenge only exists once the relay's AUTH envelope has arrived, and
	// go-nostr keeps it unexported, so there is nothing to wait on. Signing too
	// early signs an empty challenge, and Buzz then refuses every further
	// attempt on that socket with "authentication already failed" — so retrying
	// in place is worse than useless. Give the challenge a moment to land, try
	// once, and let the caller reconnect if it did not.
	select {
	case <-time.After(authSettleDelay):
	case <-ctx.Done():
		return nil
	}
	if err := relay.Auth(ctx, func(ev *nostr.Event) error { return ev.Sign(cfg.PrivateKey) }); err != nil {
		return fmt.Errorf("auth failed: %w", err)
	}
	logf("relay: authenticated")

	since := nostr.Timestamp(time.Now().Unix())
	sub, err := relay.Subscribe(ctx, nostr.Filters{{
		Kinds: []int{9},
		Tags:  nostr.TagMap{"h": []string{cfg.Channel}},
		Since: &since,
	}})
	if err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.Events:
			if !ok {
				return fmt.Errorf("subscription closed")
			}
			handleEvent(h, cfg, state, seen, ev)
		}
	}
}

func handleEvent(h *host.Host, cfg *fleet.Config, state *daemonState, seen map[string]bool, ev *nostr.Event) {
	var ptags []string
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "p" {
			ptags = append(ptags, t[1])
		}
	}
	m := fleet.Message{ID: ev.ID, PubKey: ev.PubKey, Content: ev.Content, PTags: ptags}

	if !fleet.ShouldHandle(m, seen, cfg) {
		return
	}
	seen[m.ID] = true
	state.Seen = append(state.Seen, m.ID)

	cmd, ok := fleet.ParseCommand(m, cfg)
	if !ok {
		state.save(h)
		return
	}
	logf("command from owner: %.80s", cmd.Text)
	reply := handleCommand(h, cfg, cmd.Text)
	// Short answers go inline so they are readable at a glance; long ones are
	// threaded so a screenful of journal output does not bury the channel.
	replyTo := ""
	if fleet.ShouldThread(reply) {
		replyTo = m.ID
	}
	say(h, cfg, reply, replyTo)
	state.save(h)
}

func say(h *host.Host, cfg *fleet.Config, text, replyTo string) {
	args := []string{"messages", "send", "--channel", cfg.Channel, "--content", text}
	if replyTo != "" {
		args = append(args, "--reply-to", replyTo)
	}
	if _, err := h.Buzz(cfg.PrivateKey, cfg.Relay, args...); err != nil {
		logf("send failed: %v", err)
	}
}

// ── command surface ───────────────────────────────────────────────────────────

func block(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 3000 {
		s = s[:3000]
	}
	return "```\n" + s + "\n```"
}

func handleCommand(h *host.Host, cfg *fleet.Config, text string) string {
	verb, args := fleet.ParseVerb(text)
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	hostPrefix := "**" + cfg.Host + "**\n"

	switch verb {
	case "status", "ls":
		return hostPrefix + block(capture(func() { cmdStatus(h, nil) })+"\n\n"+healthText(h, cfg))
	case "health":
		return hostPrefix + block(healthText(h, cfg))
	case "restart", "start", "stop":
		if arg == "" {
			return fmt.Sprintf("usage: `%s <agent>`", verb)
		}
		return hostPrefix + block(capture(func() { cmdLifecycle(h, verb, []string{arg}) }))
	case "logs":
		if arg == "" {
			return "usage: `logs <agent> [lines]`"
		}
		n := "25"
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil && v > 0 && v <= 80 {
				n = args[1]
			}
		}
		out := h.Journal(arg, n)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) > 30 {
			lines = lines[len(lines)-30:]
		}
		return hostPrefix + block(strings.Join(lines, "\n"))
	case "doctor":
		return hostPrefix + block(capture(func() { cmdDoctor(h) }))
	case "help", "":
		return helpText(cfg)
	default:
		return fmt.Sprintf("unknown command `%s`. Try `help`.", verb)
	}
}

func helpText(cfg *fleet.Config) string {
	broadcast := ""
	if cfg.Broadcast != "" {
		broadcast = fmt.Sprintf(", or `@%s` to reach every host at once", cfg.Broadcast)
	}
	return fmt.Sprintf("**%s** — agent supervisor on `%s`\n\n```\n"+
		"status              agents on this host: unit state, uptime, health\n"+
		"health              is each agent actually reachable on the relay\n"+
		"restart <agent>     atomic; cannot orphan a second harness\n"+
		"start|stop <agent>\n"+
		"logs <agent> [n]    recent journal lines\n"+
		"doctor              duplicates, permissions, membership\n"+
		"```\nAddress me as `@%s`%s. Only the owner is obeyed.",
		cfg.Name, cfg.Host, cfg.Name, broadcast)
}

// capture reuses the CLI's own printers for chat replies, so the two surfaces
// can never drift apart. cmdDoctor exits non-zero when it finds problems, which
// is right for a shell and wrong here, so the exit is neutralised by running
// the body in-process and restoring stdout.
func capture(fn func()) string {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return ""
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	func() {
		defer func() { _ = recover() }()
		fn()
	}()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// ── health and auto-heal ──────────────────────────────────────────────────────

func healthLoop(ctx context.Context, h *host.Host, cfg *fleet.Config, state *daemonState) {
	if cfg.NoAutoHeal {
		logf("auto-heal disabled by config")
	}
	strikes := map[string]int{}
	t := time.NewTicker(time.Duration(cfg.HealthSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick(h, cfg, state, strikes)
		}
	}
}

func tick(h *host.Host, cfg *fleet.Config, state *daemonState, strikes map[string]int) {
	now := time.Now().Unix()
	fleet.PruneHeals(state.HealHistory, now, int64(cfg.HealWindowSecs))

	for _, a := range assessAll(h, cfg) {
		if cfg.NoAutoHeal {
			continue
		}
		d := fleet.Decide(a.Name, a, strikes[a.Name], state.HealHistory, now, cfg)
		switch d.Action {
		case fleet.ActionClear:
			logf("%s: recovered before heal threshold", a.Name)
			delete(strikes, a.Name)
			delete(state.GaveUp, a.Name)
		case fleet.ActionNone:
			if _, ok := state.GaveUp[a.Name]; ok {
				logf("%s: healthy again, re-arming auto-heal", a.Name)
				delete(state.GaveUp, a.Name)
			}
		case fleet.ActionStrike:
			strikes[a.Name] = d.Strikes
			logf("%s: %s (%d/%d)", a.Name, a.State, d.Strikes, cfg.HealAfterPolls)
		case fleet.ActionGiveUp:
			strikes[a.Name] = d.Strikes
			// Said once. Repeating every tick would train you to ignore it.
			if _, ok := state.GaveUp[a.Name]; !ok {
				state.GaveUp[a.Name] = now
				logf("%s: giving up — %s", a.Name, d.Reason)
				say(h, cfg, fmt.Sprintf("**%s** — giving up on **%s**\n\n%s", cfg.Host, a.Name, d.Reason), "")
			}
		case fleet.ActionHeal:
			delete(strikes, a.Name)
			state.HealHistory[a.Name] = append(state.HealHistory[a.Name], now)
			logf("%s: %s, restarting", a.Name, a.State)
			out := capture(func() { cmdLifecycle(h, "restart", []string{a.Name}) })
			n := len(state.HealHistory[a.Name])
			extra := ""
			if n > 1 {
				extra = fmt.Sprintf("That is restart %d in the last hour.\n", n)
			}
			say(h, cfg, fmt.Sprintf(
				"**%s** — **%s** was %s (%s), so I restarted it. systemd reported `%s` throughout.\n%s%s",
				cfg.Host, a.Name, a.State, a.Detail, a.Unit, extra, block(out)), "")
		}
	}
	state.save(h)
}
