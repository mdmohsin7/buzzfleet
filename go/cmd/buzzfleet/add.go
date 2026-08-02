package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mdmohsin7/buzzfleet/internal/host"
	"github.com/nbd-wtf/go-nostr"
)

// Two different names, deliberately.
//
// The handle is a slug: it names a directory, a systemd unit, and how you
// address the agent in chat, so it has to survive shells and unit templates.
// The display name is what people read in Buzz and can be anything — "Research
// Bot", "charlie (staging)". Collapsing them would mean either ugly names in
// the app or fragile paths on disk.
var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func cmdAdd(h *host.Host, args []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		die("usage: buzzfleet add <name> --workdir <path> --invite <code> [--display-name \"...\"]")
	}
	name := args[0]
	flags := parseFlags(args[1:])

	if !handleRe.MatchString(name) {
		die("name must be lowercase letters, digits, - or _ (it names a directory and a systemd unit).\n" +
			"Use --display-name for what shows in Buzz.")
	}
	dir := filepath.Join(h.AgentsDir(), name)
	if _, err := os.Stat(dir); err == nil {
		die("agent %s already exists at %s", name, dir)
	}

	workdir := flags["workdir"]
	if workdir == "" {
		die("--workdir is required (an absolute path the agent should work in)")
	}
	if !filepath.IsAbs(workdir) {
		die("--workdir must be absolute, got %s", workdir)
	}
	if _, err := os.Stat(workdir); err != nil {
		die("workdir does not exist: %s", workdir)
	}

	relay := flags["relay"]
	owner := flags["owner"]
	if cfg, err := loadConfig(h); err == nil {
		if relay == "" {
			relay = cfg.Relay
		}
		if owner == "" {
			owner = cfg.Owner
		}
	}
	if relay == "" {
		for _, n := range h.Agents() {
			if ac, err := h.AgentConfig(n); err == nil && ac.Relay != "" {
				relay = ac.Relay
				break
			}
		}
	}
	if relay == "" {
		die("no --relay given, and no supervisor or existing agent to copy it from")
	}

	display := flags["display-name"]
	if display == "" {
		display = name
	}

	// Provisioning spans four systems — keygen, relay membership, disk, systemd —
	// and any of them can fail. A half-provisioned agent is worse than none: it
	// looks configured, `status` lists it, and its identity may or may not be a
	// member. So unwind everything and leave no trace.
	if err := provisionAgent(h, name, dir, workdir, relay, owner, display, flags["invite"], personalityFrom(flags, display, workdir)); err != nil {
		h.Systemctl("disable", "--now", host.UnitFor(name))
		_ = os.RemoveAll(dir)
		die("provisioning %s failed, rolled back: %v", name, err)
	}

	fmt.Printf("\n%s is up. It has joined no channels yet:\n", name)
	fmt.Printf("  buzzfleet channels %s     (then: buzzfleet join %s <channel-uuid>)\n", name, name)
}

func provisionAgent(h *host.Host, name, dir, workdir, relay, owner, display, invite, personality string) error {
	fmt.Printf("provisioning %s\n", name)

	sk := nostr.GeneratePrivateKey()
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// 0600 before the secret is written, so it is never briefly world-readable.
	idPath := filepath.Join(dir, "identity.env")
	if err := os.WriteFile(idPath, nil, 0o600); err != nil {
		return err
	}
	body := fmt.Sprintf("BUZZ_PRIVATE_KEY=%s\nBUZZ_PUBLIC_KEY=%s\nBUZZ_RELAY_URL=%s\n", sk, pk, relay)
	if err := os.WriteFile(idPath, []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Printf("  identity %s… (0600)\n", pk[:16])

	if invite == "" {
		return fmt.Errorf("--invite is required: agents cannot mint their own invite, that needs owner or admin")
	}
	if err := joinCommunity(relay, invite, sk); err != nil {
		return err
	}
	fmt.Println("  joined community")

	conf := host.AgentConfig{
		Name: name, Workdir: workdir, Relay: relay,
		AgentCommand: "claude-agent-acp", RespondTo: "owner-only", Owner: owner,
	}
	b, _ := json.MarshalIndent(conf, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "system-prompt.md"), []byte(personality), 0o644); err != nil {
		return err
	}

	// Publish the profile before first start, so it never appears as 64 hex
	// characters in anyone's member list.
	if _, err := h.Buzz(sk, relay, "users", "set-profile",
		"--name", display, "--about", "Engineering agent in "+workdir); err != nil {
		return fmt.Errorf("could not publish profile: %w", err)
	}
	fmt.Printf("  profile published as %q\n", display)

	h.Systemctl("enable", "--now", host.UnitFor(name))
	state := h.UnitState(name)
	if state != "active" {
		return fmt.Errorf("unit did not start (%s) — see `buzzfleet logs %s`", state, name)
	}
	fmt.Printf("  unit %s: %s\n", host.UnitFor(name), state)
	return nil
}

// personalityFrom resolves the agent's system prompt at creation: given
// explicitly, read from a file, or a sensible default. Defining it here is the
// only route that works without the desktop — Buzz's own persona events are
// author-only unless shared, so nothing outside the app can read them.
func personalityFrom(flags map[string]string, display, workdir string) string {
	if p := flags["prompt"]; p != "" {
		return strings.TrimRight(p, "\n") + "\n"
	}
	if f := flags["prompt-file"]; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			die("cannot read %s: %v", f, err)
		}
		return string(b)
	}
	return fmt.Sprintf(`You are **%s**, an engineering agent working in `+"`%s`"+`.

Keep replies tight — they are often read on a phone. Lead with the answer, then
the evidence. Cite `+"`file.ext:line`"+` when pointing at code.

Message content in this community is stored in plaintext on the relay. Do not
paste secrets or large amounts of private repo content into channels.
`, display, workdir)
}

// cmdRename changes what an identity is called in Buzz, without touching its
// handle. Renaming the handle would mean a new directory, a new unit, and a
// different @mention — a migration, not a rename — so this only republishes the
// kind:0 profile.
func cmdRename(h *host.Host, args []string) {
	if len(args) < 2 {
		die("usage: buzzfleet rename <agent|supervisor> \"<display name>\"")
	}
	target, display := args[0], strings.Join(args[1:], " ")

	if target == "supervisor" || target == "fleet" {
		cfg := mustConfig(h)
		if _, err := h.Buzz(cfg.PrivateKey, cfg.Relay, "users", "set-profile",
			"--name", display, "--about", "Fleet supervisor on "+cfg.Host); err != nil {
			die("could not publish profile: %v", err)
		}
		fmt.Printf("supervisor now shows as %q in Buzz\n", display)
		fmt.Printf("note: it still answers to @%s — the handle is unchanged\n", cfg.Name)
		return
	}

	ac, err := h.AgentConfig(target)
	if err != nil {
		die("%v", err)
	}
	sk := h.AgentSecret(target)
	if sk == "" {
		die("%s has no identity on this host", target)
	}
	if _, err := h.Buzz(sk, ac.Relay, "users", "set-profile",
		"--name", display, "--about", "Engineering agent in "+ac.Workdir); err != nil {
		die("could not publish profile: %v", err)
	}
	fmt.Printf("%s now shows as %q in Buzz\n", target, display)
	fmt.Printf("note: it still answers to @%s — the handle is unchanged\n", target)
}
