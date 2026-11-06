package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// cmdPeek runs a read-only buzz command against another of the agent's
// communities, as the agent. Run from inside a harness it knows which agent it
// is; an operator passes --agent.
func cmdPeek(h *host.Host, args []string) {
	agent := ""
	if len(args) >= 2 && args[0] == "--agent" {
		agent, args = args[1], args[2:]
	}
	self := fleet.ParseInstance(os.Getenv("BUZZ_FLEET_HARNESS"))
	if agent == "" {
		agent = self.Agent
	}
	if agent == "" || len(args) < 1 {
		die("usage: buzzfleet peek [--agent <agent>] <community> <group> <command> [flags]\nallowed: %s", fleet.PeekUsage())
	}
	community, rest := args[0], args[1:]
	if err := fleet.CheckPeek(rest); err != nil {
		die("%v", err)
	}
	c, err := h.AgentConfig(agent)
	if err != nil {
		die("%v", err)
	}
	if community == self.Community && agent == self.Agent {
		die("you are answering in %s already; use buzz directly", community)
	}
	m, ok := c.In(community)
	if !ok {
		var names []string
		for _, hn := range c.Harnesses() {
			names = append(names, hn.Community)
		}
		die("%s is not in a community called %q (it is in: %s)", agent, community, strings.Join(names, ", "))
	}
	secret := os.Getenv("BUZZ_PRIVATE_KEY")
	if secret == "" {
		secret = h.AgentSecret(agent)
	}
	if secret == "" {
		die("no identity for %s", agent)
	}
	cmd := exec.Command(filepath.Join(h.BuzzBin, "buzz"), rest...)
	if _, err := os.Stat(cmd.Path); err != nil {
		cmd = exec.Command("buzz", rest...)
	}
	env := os.Environ()
	env = setEnv(env, "BUZZ_RELAY_URL", host.HTTPRelay(m.Relay))
	env = setEnv(env, "BUZZ_PRIVATE_KEY", secret)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		die("%v", err)
	}
}

// ── what a harness is told about its communities ──────────────────────────────

func sharedNotesPath(h *host.Host, agent string) string {
	return filepath.Join(h.AgentsDir(), agent, "shared-notes.md")
}

// addendumPath is a community's own addition to the agent's personality:
// agents/<agent>/system-prompt.<community>.md.
func addendumPath(h *host.Host, agent, community string) string {
	return filepath.Join(h.AgentsDir(), agent, "system-prompt."+community+".md")
}

// harnessPrompt builds the system prompt a multi-community harness runs with:
// the agent's personality, this community's addendum, and where it is. It is
// written under the run directory, private to the agent's user, and its path
// is what the harness is started with. An agent in one community keeps using
// its system-prompt.md directly, exactly as before.
func harnessPrompt(h *host.Host, c *host.AgentConfig, hn fleet.Harness) (string, []string, error) {
	var parts []string
	if b, err := os.ReadFile(promptPath(h, c.Name)); err == nil && strings.TrimSpace(string(b)) != "" {
		parts = append(parts, strings.TrimSpace(string(b)))
	}
	if b, err := os.ReadFile(addendumPath(h, c.Name, hn.Community)); err == nil && strings.TrimSpace(string(b)) != "" {
		parts = append(parts, strings.TrimSpace(string(b)))
	}
	var names []string
	for _, x := range c.Harnesses() {
		names = append(names, x.Community)
	}
	exe, _ := os.Executable()
	notes := sharedNotesPath(h, c.Name)
	if _, err := os.Stat(notes); os.IsNotExist(err) {
		_ = os.WriteFile(notes, []byte("# Shared notes\n\nDurable facts useful in every community this agent is in. Nothing community-private.\n"), 0o640)
	}
	parts = append(parts, fleet.CommunitiesPrompt(hn.Community, names, exe+" peek", notes))

	if err := host.EnsureRunDir(h.RunDir()); err != nil {
		return "", nil, err
	}
	path := filepath.Join(h.RunDir(), "prompt-"+hn.Instance()+".md")
	if err := os.WriteFile(path+".tmp", []byte(strings.Join(parts, "\n\n")+"\n"), 0o600); err != nil {
		return "", nil, err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return "", nil, err
	}
	type entry struct {
		Name    string `json:"name"`
		Current bool   `json:"current"`
	}
	var list []entry
	for _, n := range names {
		list = append(list, entry{n, n == hn.Community})
	}
	js, _ := json.Marshal(list)
	return path, []string{
		"BUZZ_FLEET_COMMUNITIES=" + string(js),
		"BUZZ_FLEET_SHARED_NOTES=" + notes,
	}, nil
}

func describeCommunityPrompt(h *host.Host, agent, community string) string {
	b, err := os.ReadFile(addendumPath(h, agent, community))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return fmt.Sprintf("%s@%s has no addendum; it runs with %s's personality alone.\n", agent, community, agent)
	}
	return string(b)
}
