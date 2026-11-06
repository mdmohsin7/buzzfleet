package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// An agent in another community is a second harness on the same identity,
// workdir and model login. See docs/design/multi-community.md.

func cmdCommunity(h *host.Host, args []string) {
	if len(args) == 0 {
		die("usage: buzzfleet community add|rm|ls ...")
	}
	switch args[0] {
	case "add":
		cmdCommunityAdd(h, args[1:])
	case "rm", "remove":
		cmdCommunityRm(h, args[1:])
	case "ls", "list":
		cmdCommunityLs(h, args[1:])
	default:
		die("unknown community command %q: add, rm or ls", args[0])
	}
}

const communityAddUsage = `usage: buzzfleet community add <agent> <name> --invite <code> --relay <url> --access full
          [--owner <hex>] [--respond-to <mode>]`

// splitFlags reads --key value pairs and returns the positional arguments too.
func splitFlags(args []string) (map[string]string, []string) {
	flags := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			k := strings.TrimPrefix(args[i], "--")
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[k] = args[i+1]
				i++
			} else {
				flags[k] = ""
			}
			continue
		}
		pos = append(pos, args[i])
	}
	return flags, pos
}

// communitiesJSON renders the communities map for splicing into agent.json,
// indented to sit at the top level of a two-space-indented file.
func communitiesJSON(cs map[string]fleet.CommunityConfig) ([]byte, error) {
	return json.MarshalIndent(cs, "  ", "  ")
}

func writeCommunities(h *host.Host, agent string, cs map[string]fleet.CommunityConfig) error {
	raw, err := h.RawAgentConfig(agent)
	if err != nil {
		return err
	}
	var out []byte
	if len(cs) == 0 {
		out, err = fleet.DeleteTopLevelKey(raw, "communities")
	} else {
		var v []byte
		if v, err = communitiesJSON(cs); err == nil {
			out, err = fleet.SetTopLevelKey(raw, "communities", v)
		}
	}
	if err != nil {
		return fmt.Errorf("cannot edit agent.json: %w", err)
	}
	return h.WriteRawAgentConfig(agent, out)
}

// ── drop-ins ──────────────────────────────────────────────────────────────────
//
// Per-agent unit settings (model, environment, queueing) live in the main
// harness's drop-in directory. Every other harness of the agent links to the
// same files, so they cannot drift apart and there is one place to edit.

func unitDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config/systemd/user")
}

func dropInDir(instance string) string {
	return filepath.Join(unitDir(), host.UnitFor(instance)+".d")
}

// mainDropIns lists the .conf files in the agent's main drop-in directory.
func mainDropIns(agent string) []string {
	matches, _ := filepath.Glob(filepath.Join(dropInDir(agent), "*.conf"))
	sort.Strings(matches)
	return matches
}

// linkDropIns links every main drop-in into a community harness's directory.
// It returns what it created, so a failed add can remove exactly that.
func linkDropIns(agent, instance string) ([]string, error) {
	dir := dropInDir(instance)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var made []string
	for _, src := range mainDropIns(agent) {
		dst := filepath.Join(dir, filepath.Base(src))
		if cur, err := os.Readlink(dst); err == nil && cur == src {
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			return made, fmt.Errorf("link drop-in %s: %w", filepath.Base(src), err)
		}
		made = append(made, dst)
	}
	return made, nil
}

// unlinkDropIns removes a community harness's directory, but only if it holds
// nothing except links into the main one; anything else was put there by
// someone, and is reported rather than deleted.
func unlinkDropIns(agent, instance string) error {
	dir := dropInDir(instance)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	mainDir := dropInDir(agent)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		target, err := os.Readlink(p)
		if err != nil || filepath.Dir(target) != mainDir {
			return fmt.Errorf("%s holds %s, which is not a link to the main drop-ins; leaving the directory in place", dir, e.Name())
		}
	}
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return os.Remove(dir)
}

// ── add, rm, ls ───────────────────────────────────────────────────────────────

func cmdCommunityAdd(h *host.Host, args []string) {
	flags, pos := splitFlags(args)
	if len(pos) != 2 {
		die(communityAddUsage)
	}
	agent, name := pos[0], pos[1]
	conf, err := h.AgentConfig(agent)
	if err != nil {
		die("%v", err)
	}
	if err := fleet.ValidAgentName(agent); err != nil {
		die("%v", err)
	}
	if err := fleet.ValidCommunityName(name); err != nil {
		die("%v", err)
	}
	if _, exists := conf.Communities[name]; exists {
		die("%s is already in a community called %q", agent, name)
	}
	switch flags["access"] {
	case fleet.AccessFull:
	case "":
		die("--access is required. `full` lets this community's harness see everything the agent sees: " +
			"its whole workdir, its coding agent's memory, and its message history from other communities.")
	case "restricted":
		die("--access restricted is designed but not built yet (docs/design/multi-community.md, section 5)")
	default:
		die("--access must be full")
	}
	if flags["invite"] == "" || flags["relay"] == "" {
		die(communityAddUsage)
	}
	sk := h.AgentSecret(agent)
	if sk == "" {
		die("%s has no identity on this host", agent)
	}

	next := map[string]fleet.CommunityConfig{}
	for k, v := range conf.Communities {
		next[k] = v
	}
	next[name] = fleet.CommunityConfig{
		Relay: flags["relay"], Owner: flags["owner"], RespondTo: flags["respond-to"],
		Access: fleet.AccessFull, JoinedAt: time.Now().Unix(),
	}
	// Checked before anything is touched: the same relay twice would give one
	// identity two harnesses there, and both would answer every mention.
	if err := fleet.CheckCommunities(conf.Relay, next); err != nil {
		die("%v", err)
	}

	inst := fleet.Harness{Agent: agent, Community: name}.Instance()
	raw, _ := h.RawAgentConfig(agent)
	// Prove the config can be edited before joining anything: a hand-edited
	// file that cannot be changed cleanly should stop the add here, not after
	// the invite is spent.
	if v, err := communitiesJSON(next); err != nil {
		die("%v", err)
	} else if _, err := fleet.SetTopLevelKey(raw, "communities", v); err != nil {
		die("cannot edit agent.json: %v", err)
	}
	var links []string
	// Undo, in reverse, whatever got done. Joining the community cannot be
	// undone from here; it is harmless, and said.
	fail := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "buzzfleet: "+format+"\n", a...)
		h.Systemctl("disable", "--now", host.UnitFor(inst))
		for _, l := range links {
			_ = os.Remove(l)
		}
		_ = os.Remove(dropInDir(inst))
		if raw != nil {
			_ = h.WriteRawAgentConfig(agent, raw)
		}
		h.Systemctl("daemon-reload")
		fmt.Fprintf(os.Stderr, "buzzfleet: rolled back. %s may still be a member of the community; that is harmless.\n", agent)
		os.Exit(1)
	}

	if err := joinCommunity(flags["relay"], flags["invite"], sk); err != nil {
		die("could not join: %v", err)
	}
	fmt.Println("  joined community")

	// The same face everywhere: copy the profile it already has.
	prof := ownProfile(h, sk, conf.Relay)
	if prof.name == "" {
		prof.name = agent
	}
	profArgs := []string{"users", "set-profile", "--name", prof.name}
	if prof.about != "" {
		profArgs = append(profArgs, "--about", prof.about)
	}
	if prof.avatar != "" {
		profArgs = append(profArgs, "--avatar", prof.avatar)
	}
	if _, err := h.Buzz(sk, flags["relay"], profArgs...); err != nil {
		fail("could not publish profile in the new community: %v", err)
	}
	fmt.Printf("  profile published as %q\n", prof.name)

	if err := writeCommunities(h, agent, next); err != nil {
		fail("%v", err)
	}
	if links, err = linkDropIns(agent, inst); err != nil {
		fail("%v", err)
	}
	if len(links) > 0 {
		fmt.Printf("  linked %d drop-in(s) from %s\n", len(links), host.UnitFor(agent))
	}
	h.Systemctl("daemon-reload")
	h.Systemctl("enable", "--now", host.UnitFor(inst))
	// A harness that exits a second after starting is still "active" at
	// first glance; give it a moment to show what it really does.
	time.Sleep(3 * time.Second)
	if st := h.UnitState(inst); st != "active" {
		fail("unit %s did not start (%s); see `buzzfleet logs %s@%s`", host.UnitFor(inst), st, agent, name)
	}
	fmt.Printf("  unit %s: active\n", host.UnitFor(inst))
	fmt.Printf("\n%s@%s is up. It has joined no channels there yet:\n", agent, name)
	fmt.Printf("  buzzfleet channels %s@%s     (then: buzzfleet join %s@%s <channel-uuid>)\n", agent, name, agent, name)
	fmt.Println("  (its Docker proxy socket appears within a minute, once the supervisor next looks)")
	if len(conf.Communities) == 0 {
		fmt.Printf("\nOne more step: %s's original harness is not behind the turn gate yet, so for now\n"+
			"its turns can overlap %s@%s's. Restart it when it is idle:\n  buzzfleet restart %s@main\n",
			agent, agent, name, agent)
	}
}

type profile struct{ name, about, avatar string }

// ownProfile reads the agent's profile from its main community, falling back
// to its name: a member list should never show 64 hex characters.
func ownProfile(h *host.Host, secret, relay string) profile {
	p := profile{}
	if out, err := h.Buzz(secret, relay, "users", "get"); err == nil {
		var rows []map[string]any
		if json.Unmarshal([]byte(out), &rows) == nil && len(rows) > 0 {
			str := func(k string) string { s, _ := rows[0][k].(string); return s }
			p.name = str("display_name")
			if p.name == "" {
				p.name = str("name")
			}
			p.about, p.avatar = str("about"), str("picture")
		}
	}
	return p
}

func cmdCommunityRm(h *host.Host, args []string) {
	_, pos := splitFlags(args)
	if len(pos) != 2 {
		die("usage: buzzfleet community rm <agent> <name>")
	}
	agent, name := pos[0], pos[1]
	conf, err := h.AgentConfig(agent)
	if err != nil {
		die("%v", err)
	}
	if name == fleet.MainCommunity {
		die("the community %s was created in cannot be removed; remove the agent instead", agent)
	}
	if _, ok := conf.Communities[name]; !ok {
		die("%s is not in a community called %q", agent, name)
	}
	inst := fleet.Harness{Agent: agent, Community: name}.Instance()
	h.Systemctl("disable", "--now", host.UnitFor(inst))
	fmt.Printf("  stopped and disabled %s\n", host.UnitFor(inst))
	if err := unlinkDropIns(agent, inst); err != nil {
		fmt.Fprintf(os.Stderr, "  ! %v\n", err)
	}
	// Its gate is gone with it; so is any record of it waiting.
	_ = os.Remove(h.TurnWaitPath(inst))
	_ = os.RemoveAll(filepath.Join(h.RunDir(), "shim", inst))
	next := map[string]fleet.CommunityConfig{}
	for k, v := range conf.Communities {
		if k != name {
			next[k] = v
		}
	}
	if err := writeCommunities(h, agent, next); err != nil {
		die("%v", err)
	}
	h.Systemctl("daemon-reload")
	fmt.Printf("%s no longer runs in %q. It is still a member there; its messages there stay.\n", agent, name)
	// Its other harnesses still list the old set of communities, and run
	// behind the gate, until they restart.
	if len(next) == 0 {
		fmt.Printf("Restart %s when it is idle, so it runs as a single-community agent again: buzzfleet restart %s\n", agent, agent)
	} else {
		fmt.Printf("Restart its other harnesses when idle, so their prompts stop listing %q: buzzfleet restart %s\n", name, agent)
	}
}

func cmdCommunityLs(h *host.Host, args []string) {
	agents := args
	if len(agents) == 0 {
		agents = h.Agents()
	}
	fmt.Printf("%-16s%-8s%-10s%s\n", "HARNESS", "ACCESS", "UNIT", "RELAY")
	for _, a := range agents {
		for _, t := range mustResolve(h, a) {
			access := fleet.AccessFull
			if cc, ok := t.Config.Communities[t.Community]; ok {
				access = cc.Access
			}
			fmt.Printf("%-16s%-8s%-10s%s\n", t.Harness.Label(true), access, h.UnitState(t.Instance()), t.Relay)
		}
	}
}

// communityFindings is doctor's view of an agent's extra communities.
func communityFindings(h *host.Host, c *host.AgentConfig, reachable bool) []finding {
	var out []finding
	if len(c.Communities) == 0 {
		return nil
	}
	if err := fleet.CheckCommunities(c.Relay, c.Communities); err != nil {
		out = append(out, finding{msg: fmt.Sprintf("%s: %v", c.Name, err)})
	}
	// With more than one harness on one workdir, every one of them must go
	// through the turn gate, or two turns can run at once.
	for _, hn := range c.Harnesses() {
		pid := h.MainPID(hn.Instance())
		if pid == "" {
			continue
		}
		env, _ := os.ReadFile("/proc/" + pid + "/environ")
		vars := "\x00" + string(env)
		switch {
		case strings.Contains(vars, "\x00"+envNoGate+"="):
			out = append(out, finding{warn: true, msg: fmt.Sprintf("%s: turn gate turned off (%s); its turns can overlap the agent's other communities", hn.Label(true), envNoGate)})
		case !strings.Contains(vars, "\x00"+envGateReal+"="):
			out = append(out, finding{msg: fmt.Sprintf("%s: started without the turn gate, so its turns can overlap the agent's other communities — restart it", hn.Label(true))})
		}
	}
	for _, hn := range c.Harnesses()[1:] {
		inst, label := hn.Instance(), hn.Label(true)
		on := h.UnitEnabled(inst)
		out = append(out, finding{ok: on, msg: fmt.Sprintf("%s: %s", label,
			map[bool]string{true: "starts on boot", false: "NOT enabled — will not come back after a reboot"}[on])})
		for _, src := range mainDropIns(c.Name) {
			dst := filepath.Join(dropInDir(inst), filepath.Base(src))
			if cur, err := os.Readlink(dst); err != nil || cur != src {
				out = append(out, finding{msg: fmt.Sprintf("%s: drop-in %s is not linked from %s, so this harness runs without it — `buzzfleet community add` links it; link it by hand with ln -s",
					label, filepath.Base(src), host.UnitFor(c.Name))})
			}
		}
		if reachable {
			m, _ := c.In(hn.Community)
			if chans, err := h.Channels(h.AgentSecret(c.Name), m.Relay); err != nil {
				out = append(out, finding{msg: fmt.Sprintf("%s: cannot reach that community's relay as this identity (%v)", label, err)})
			} else {
				out = append(out, finding{ok: len(chans) > 0, msg: fmt.Sprintf("%s: member of %d channel(s)%s", label, len(chans),
					map[bool]string{true: "", false: " — it will sit idle until it joins one"}[len(chans) > 0])})
			}
		}
	}
	return out
}
