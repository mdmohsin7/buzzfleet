// buzzfleet — run and supervise Buzz agents on your own servers.
//
// One static binary. `buzzfleet daemon` is the supervisor that joins your
// community and answers in a channel, which is what makes fleet control work
// from mobile, desktop and web without any client UI; the rest are the local
// commands it drives.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const usage = `buzzfleet — run and supervise Buzz agents on this machine

  status [name...]              agents: unit state, uptime, relay health
  health                        is each agent actually present on the relay
  start|stop|restart <agent>[@community]
                                lifecycle (systemd; restart cannot orphan)
  logs <agent>[@community] [-n N]  recent journal output
  doctor                        duplicates, permissions, membership
  reap [--dry-run]              kill harnesses systemd is not supervising
  limits [--apply]              cap the CPU and memory all agents can use
  community add <agent> <name> --invite <code> --relay <url> --access full
                                run the agent in another community too
  community rm <agent> <name> | community ls [agent]
  heartbeat [--send]            show, or send once, the out-of-band heartbeat
  add <name> --workdir <path> --invite <code> [--display-name "..."]
       [--prompt "..." | --prompt-file <path>]
                                provision a new agent end to end
  prompt <agent> [--set "..." | --file <path> | --edit]
                                show or change an agent's personality
  rename <agent|supervisor> "<display name>"
                                change what it is called in Buzz
  channels <agent>              channels that agent can see
  join <agent> <channel-uuid>   join one
  install [--force]             write the systemd units and check this host
  init [--invite <code>]        set up the chat supervisor
  daemon                        run the supervisor (normally via systemd)
  run-agent <name>              launch one harness (called by systemd)
`

// Set by the release build via -ldflags.
var version = "dev"

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "buzzfleet: "+format+"\n", a...)
	os.Exit(1)
}

func fleetHome() string {
	if v := os.Getenv("FLEET_HOME"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err == nil {
		if abs, err := filepath.Abs(filepath.Join(filepath.Dir(exe), "..")); err == nil {
			if _, err := os.Stat(filepath.Join(abs, "agents")); err == nil {
				return abs
			}
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "buzzfleet")
}

func derivePubkey(secret string) (string, error) {
	if strings.HasPrefix(secret, "nsec") {
		_, data, err := nip19.Decode(secret)
		if err != nil {
			return "", err
		}
		secret = data.(string)
	}
	return nostr.GetPublicKey(secret)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return
	}
	h := host.New(fleetHome())
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "status", "ls":
		cmdStatus(h, args)
	case "health":
		fmt.Println(healthText(h, mustConfig(h)))
	case "start", "stop", "restart":
		cmdLifecycle(h, cmd, args)
	case "logs":
		cmdLogs(h, args)
	case "doctor":
		cmdDoctor(h)
	case "reap":
		cmdReap(h, args)
	case "limits":
		cmdLimits(h, args)
	case "community":
		cmdCommunity(h, args)
	case "heartbeat":
		cmdHeartbeat(h, args)
	case "channels":
		cmdChannels(h, args)
	case "join":
		cmdJoin(h, args)
	case "install":
		cmdInstall(h, args)
	case "run-agent":
		cmdRunAgent(h, args)
	case "add":
		cmdAdd(h, args)
	case "prompt":
		cmdPrompt(h, args)
	case "rename":
		cmdRename(h, args)
	case "init":
		cmdInit(h, args)
	case "daemon":
		runDaemon(h, mustConfig(h))
	case "version", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
}

// ── config ────────────────────────────────────────────────────────────────────

func configPath(h *host.Host) string {
	if v := os.Getenv("BUZZFLEETD_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(h.FleetHome, "fleetd.json")
}

func loadConfig(h *host.Host) (*fleet.Config, error) {
	b, err := os.ReadFile(configPath(h))
	if err != nil {
		return nil, err
	}
	var c fleet.Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return c.WithDefaults(), nil
}

func mustConfig(h *host.Host) *fleet.Config {
	c, err := loadConfig(h)
	if err != nil {
		die("no supervisor config at %s — run `buzzfleet init` first", configPath(h))
	}
	return c
}

// ── status and health ─────────────────────────────────────────────────────────

func cmdStatus(h *host.Host, args []string) {
	var ts []target
	if len(args) == 0 {
		ts = allTargets(h)
	}
	for _, a := range args {
		ts = append(ts, mustResolve(h, a)...)
	}
	if len(ts) == 0 {
		fmt.Println("no agents configured")
		return
	}
	fmt.Printf("%-16s%-10s%-6s%-9s%-7s%s\n", "AGENT", "STATE", "BOOT", "PID", "UP", "WORKDIR")
	for _, t := range ts {
		inst := t.Instance()
		state := h.UnitState(inst)
		pid := h.MainPID(inst)
		if pid == "" {
			pid = "-"
		}
		up := "-"
		if state == "active" {
			up = h.Uptime(inst)
		}
		boot := "no"
		if h.UnitEnabled(inst) {
			boot = "yes"
		}
		wd := "-"
		if t.Config != nil {
			wd = t.Config.Workdir
		}
		fmt.Printf("%-16s%-10s%-6s%-9s%-7s%s\n", t.Label, state, boot, pid, up, wd)
	}
}

// assessAll gathers one reading per harness, with one relay call per
// community for the lot.
//
// Presence lives on each community's relay. The supervisor's own identity is
// only a member of the community it was set up in, so presence elsewhere is
// asked as one of the agents that is a member there: any member may read it.
func assessAll(h *host.Host, cfg *fleet.Config) []fleet.Assessment {
	ts := allTargets(h)
	pubkeys := map[string]string{} // agent -> pubkey
	type relayQuery struct {
		asSecret string
		pubkeys  []string
	}
	queries := map[string]*relayQuery{} // canonical relay -> query
	mainRelay := fleet.CanonicalRelay(cfg.Relay)
	for _, t := range ts {
		pk, ok := pubkeys[t.Agent]
		if !ok {
			pk = h.AgentPubkey(t.Agent, derivePubkey)
			pubkeys[t.Agent] = pk
		}
		if pk == "" || t.Relay == "" {
			continue
		}
		r := fleet.CanonicalRelay(t.Relay)
		q := queries[r]
		if q == nil {
			q = &relayQuery{}
			if r == mainRelay {
				q.asSecret = cfg.PrivateKey
			} else {
				q.asSecret = h.AgentSecret(t.Agent)
			}
			queries[r] = q
		}
		q.pubkeys = append(q.pubkeys, pk)
	}
	type answer struct {
		online map[string]bool
		err    error
	}
	answers := map[string]answer{}
	for r, q := range queries {
		relay := r
		if r == mainRelay {
			relay = cfg.Relay
		} else {
			for _, t := range ts {
				if fleet.CanonicalRelay(t.Relay) == r {
					relay = t.Relay
					break
				}
			}
		}
		online, err := h.PresenceOf(q.asSecret, relay, q.pubkeys)
		answers[r] = answer{online, err}
	}

	// One model login per agent: a usage limit or a dead login is the
	// agent's, so it is read once and shown on every one of its harnesses.
	inference := map[string][2]string{}
	out := make([]fleet.Assessment, 0, len(ts))
	for _, t := range ts {
		inst := t.Instance()
		unit := h.UnitState(inst)
		pk := pubkeys[t.Agent]
		hasID := pk != ""

		present := fleet.TriUnknown
		if ans, ok := answers[fleet.CanonicalRelay(t.Relay)]; ok && ans.err == nil && hasID {
			if ans.online[pk] {
				present = fleet.TriYes
			} else {
				present = fleet.TriNo
			}
		}
		child := fleet.TriUnknown
		if unit == "active" && present == fleet.TriYes {
			if alive, known := h.ChildAlive(inst); known {
				if alive {
					child = fleet.TriYes
				} else {
					child = fleet.TriNo
				}
			}
		}
		a := fleet.Assess(t.Label, unit, present, child, hasID)
		// Only worth reading transcripts for an agent that otherwise looks
		// fine: that is the one whose failing turns nothing else would catch.
		if a.State == fleet.StateOK {
			inf, done := inference[t.Agent]
			if !done {
				st, detail := fleet.InferenceState(fleet.Latest(h.InferenceSignals(t.Agent)), time.Now())
				inf = [2]string{string(st), detail}
				inference[t.Agent] = inf
			}
			a = fleet.WithInference(a, fleet.State(inf[0]), inf[1])
		}
		out = append(out, a)
	}
	return out
}

func healthText(h *host.Host, cfg *fleet.Config) string {
	rows := assessAll(h, cfg)
	if len(rows) == 0 {
		return "no agents configured"
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-16s%-9s%s\n", r.Name, r.State, r.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ── lifecycle ─────────────────────────────────────────────────────────────────

func cmdLifecycle(h *host.Host, verb string, args []string) {
	if len(args) == 0 {
		die("usage: buzzfleet %s <agent>[@community]", verb)
	}
	for _, t := range mustResolve(h, args[0]) {
		lifecycleOne(h, verb, t)
	}
}

func lifecycleOne(h *host.Host, verb string, t target) {
	inst := t.Instance()
	h.Systemctl(verb, host.UnitFor(inst))
	state := h.UnitState(inst)

	if verb != "restart" {
		fmt.Printf("%s: %s\n", t.Label, state)
		return
	}
	// systemd tears down the whole control group, so a restart cannot leave the
	// old harness alive — but verify rather than trust, because assuming a kill
	// worked is exactly what produced duplicate replies by hand.
	var strays []string
	for _, p := range h.Harnesses(derivePubkey) {
		if p.Supervised || p.Agent != t.Agent {
			continue
		}
		// A stray counts against this harness if it says it is this harness,
		// or if it is on this harness's relay.
		if p.Instance == inst || (p.Instance == "" && (p.Relay == "" || p.Relay == fleet.CanonicalRelay(t.Relay))) {
			strays = append(strays, p.PID)
		}
	}
	if len(strays) > 0 {
		fmt.Printf("%s: %s, but %d unsupervised harness(es) remain (%s) — run `buzzfleet doctor`\n",
			t.Label, state, len(strays), strings.Join(strays, ", "))
		return
	}
	fmt.Printf("%s: %s, single harness confirmed\n", t.Label, state)
}

func cmdLogs(h *host.Host, args []string) {
	if len(args) == 0 {
		die("usage: buzzfleet logs <agent>[@community] [-n N]")
	}
	n := "40"
	for i, a := range args {
		if a == "-n" && i+1 < len(args) {
			n = args[i+1]
		}
	}
	fmt.Println(h.Journal(mustOne(h, args[0], "logs").Instance(), n))
}

// ── doctor and reap ───────────────────────────────────────────────────────────

func cmdDoctor(h *host.Host) {
	problems := 0
	say := func(ok bool, msg string) {
		tag := "ok  "
		if !ok {
			tag = "FAIL"
			problems++
		}
		fmt.Printf("%s %s\n", tag, msg)
	}

	// One identity on one relay must have exactly one harness. The same
	// identity on two relays is one agent in two communities, which is fine.
	byKey := map[string][]host.Proc{}
	var keys []string
	procs := h.Harnesses(derivePubkey)
	for _, p := range procs {
		k := p.DuplicateKey()
		if _, seen := byKey[k]; !seen {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], p)
	}
	clean := true
	for _, k := range keys {
		list := byKey[k]
		who := list[0].Agent
		if list[0].Instance != "" {
			who = fleet.ParseInstance(list[0].Instance).Label(true)
		}
		if len(list) > 1 {
			clean = false
			say(false, fmt.Sprintf("%s: %d harnesses on one identity and one relay — every mention will be answered %dx", who, len(list), len(list)))
		}
		var loose []string
		for _, p := range list {
			if !p.Supervised {
				loose = append(loose, p.PID)
			}
		}
		if len(loose) > 0 {
			clean = false
			say(false, fmt.Sprintf("%s: %d harness(es) not under systemd (%s) — a restart will not stop them",
				who, len(loose), strings.Join(loose, ", ")))
		}
	}
	if clean {
		say(true, fmt.Sprintf("%d harness process(es), all supervised, none duplicated", len(procs)))
	}

	// Ceilings, as the kernel applies them: systemd accepts limits it cannot
	// enforce without a word, so the unit files are not evidence.
	mode, delegated := host.CgroupMode()
	target := fleet.PlanLimits(mode, delegated, os.Getuid(), hostLimits(h))
	for _, f := range append(enforcementLines(h, mode, target), dockerFindings(h)...) {
		if f.warn {
			fmt.Printf("WARN %s\n", f.msg)
			continue
		}
		say(f.ok, f.msg)
	}

	_, cfgErr := loadConfig(h)
	seen := map[string][]string{}
	for _, n := range h.Agents() {
		c, err := h.AgentConfig(n)
		if err != nil {
			say(false, fmt.Sprintf("%s: %v", n, err))
			continue
		}
		_, statErr := os.Stat(c.Workdir)
		say(statErr == nil, fmt.Sprintf("%s: workdir %s", n, c.Workdir))

		mode := h.IdentityMode(n)
		if mode == "" {
			say(false, n+": identity.env missing")
		} else {
			say(mode == "600", fmt.Sprintf("%s: identity.env mode %s%s", n, mode,
				map[bool]string{true: "", false: " (should be 600 — it holds the private key)"}[mode == "600"]))
		}
		say(h.UnitEnabled(n), fmt.Sprintf("%s: %s", n,
			map[bool]string{true: "starts on boot", false: "NOT enabled — will not come back after a reboot"}[h.UnitEnabled(n)]))

		if pk := h.AgentPubkey(n, derivePubkey); pk != "" {
			seen[pk] = append(seen[pk], n)
		}
		if cfgErr == nil {
			if chans, err := h.Channels(h.AgentSecret(n), c.Relay); err != nil {
				say(false, fmt.Sprintf("%s: cannot reach relay as this identity (%v)", n, err))
			} else {
				say(len(chans) > 0, fmt.Sprintf("%s: member of %d channel(s)%s", n, len(chans),
					map[bool]string{true: "", false: " — it will sit idle until it joins one"}[len(chans) > 0]))
			}
		}
		for _, f := range communityFindings(h, c, cfgErr == nil) {
			say(f.ok, f.msg)
		}
	}
	// Two agents on one key both answer every mention, exactly like two
	// harnesses on one key.
	for pk, names := range seen {
		if len(names) > 1 {
			say(false, fmt.Sprintf("agents %s share pubkey %s… — they will both answer every mention",
				strings.Join(names, ", "), pk[:12]))
		}
	}

	if problems == 0 {
		fmt.Println("\nno problems found")
		return
	}
	fmt.Printf("\n%d problem(s)\n", problems)
	os.Exit(1)
}

func cmdReap(h *host.Host, args []string) {
	dry := len(args) > 0 && args[0] == "--dry-run"
	var loose []host.Proc
	for _, p := range h.Harnesses(derivePubkey) {
		if !p.Supervised {
			loose = append(loose, p)
		}
	}
	if len(loose) == 0 {
		fmt.Println("no unsupervised harnesses")
		return
	}
	for _, p := range loose {
		if dry {
			fmt.Printf("would kill %s (%s)\n", p.PID, p.Agent)
			continue
		}
		pid, err := strconv.Atoi(p.PID)
		if err != nil {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		_ = proc.Signal(os.Interrupt)
		fmt.Printf("killed %s (%s)\n", p.PID, p.Agent)
	}
}

// ── channels ──────────────────────────────────────────────────────────────────

func cmdChannels(h *host.Host, args []string) {
	if len(args) == 0 {
		die("usage: buzzfleet channels <agent>[@community]")
	}
	t := mustOne(h, args[0], "channels")
	chans, err := h.Channels(h.AgentSecret(t.Agent), t.Relay)
	if err != nil {
		die("%v", err)
	}
	for _, ch := range chans {
		fmt.Printf("%s  %s  %s\n", ch.ID, ch.Name, ch.Description)
	}
}

func cmdJoin(h *host.Host, args []string) {
	if len(args) < 2 {
		die("usage: buzzfleet join <agent>[@community] <channel-uuid>")
	}
	t := mustOne(h, args[0], "join")
	out, err := h.Buzz(h.AgentSecret(t.Agent), t.Relay, "channels", "join", "--channel", args[1])
	if err != nil {
		die("join failed: %v", err)
	}
	fmt.Printf("%s joined %s\n%s\n", t.Label, args[1], out)
}
