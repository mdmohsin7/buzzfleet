package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// Everything buzzfleet needs is in this binary: the units are written from
// templates below, and `run-agent` replaces the shell launcher. That is what
// makes a one-command install possible — there is nothing to clone, and no
// scripts that have to sit in the right place next to it.

const agentUnit = `[Unit]
Description=Buzz agent %%i
Documentation=https://github.com/mdmohsin7/buzzfleet
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
Environment=FLEET_HOME=%s
ExecStart=%s run-agent %%i
# Named, so the fleet-wide ceiling from "buzzfleet limits" has a slice to go on.
Slice=%s
Restart=on-failure
RestartSec=5s

# systemd owns the whole process tree, so a restart cannot leave an orphaned
# harness behind still holding the agent's identity and answering mentions.
# That double-reply is the exact failure this unit exists to make impossible.
KillMode=control-group
TimeoutStopSec=30

[Install]
WantedBy=default.target
`

const supervisorUnit = `[Unit]
Description=Buzz fleet supervisor
Documentation=https://github.com/mdmohsin7/buzzfleet
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=600
StartLimitBurst=10

[Service]
Type=simple
Environment=FLEET_HOME=%s
ExecStart=%s daemon
Restart=always
RestartSec=10s

[Install]
WantedBy=default.target
`

func agentUnitBody(fleetHome, exe string) string {
	return fmt.Sprintf(agentUnit, fleetHome, exe, fleet.AgentSlice)
}

// cmdInstall writes the systemd units and checks the two things that are easy
// to get wrong and miserable to debug later.
func cmdInstall(h *host.Host, args []string) {
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
	}

	exe, err := os.Executable()
	if err != nil {
		die("cannot resolve my own path: %v", err)
	}
	exe, _ = filepath.Abs(exe)

	home, _ := os.UserHomeDir()
	unitDir := filepath.Join(home, ".config/systemd/user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		die("cannot create %s: %v", unitDir, err)
	}
	if err := os.MkdirAll(h.AgentsDir(), 0o755); err != nil {
		die("cannot create %s: %v", h.AgentsDir(), err)
	}

	units := map[string]string{
		"buzz-agent@.service": agentUnitBody(h.FleetHome, exe),
		"buzzfleetd.service":  fmt.Sprintf(supervisorUnit, h.FleetHome, exe),
	}
	for name, body := range units {
		path := filepath.Join(unitDir, name)
		// Never silently replace a unit that someone has changed, or that points
		// somewhere else: agents are running off these, and rewriting one with a
		// different fleet home stops them at the next restart — quietly, because
		// nothing fails until then.
		if existing, err := os.ReadFile(path); err == nil && string(existing) != body && !force {
			die("%s already exists and differs from what I would write.\n"+
				"Something is probably running off it. Re-run with --force to replace it,\n"+
				"or use a drop-in at %s.d/override.conf to change it safely.", path, path)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			die("cannot write %s: %v", name, err)
		}
		fmt.Printf("  unit %s\n", name)
	}
	h.Systemctl("daemon-reload")
	fmt.Printf("  fleet home %s\n", h.FleetHome)

	warn := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "  ! "+format+"\n", a...)
	}

	// Without XDG_RUNTIME_DIR, `systemctl --user` fails with the famously
	// unhelpful "Failed to connect to bus: No medium found".
	if out := h.Systemctl("is-system-running"); strings.Contains(out, "medium") || out == "" {
		warn("no systemd user session — agents cannot be supervised here")
	}

	// Without lingering, the user manager stops at logout and every agent dies
	// with it. The failure looks like "it worked until I closed my terminal".
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	if out, _ := exec.Command("loginctl", "show-user", user, "-p", "Linger", "--value").Output(); strings.TrimSpace(string(out)) != "yes" {
		warn("lingering is off — agents will not survive logout or reboot")
		warn("  fix: sudo loginctl enable-linger %s", user)
	}

	// buzzfleet drives the buzz CLI for relay writes rather than reimplementing
	// signing, so one has to exist somewhere.
	if _, err := exec.LookPath(filepath.Join(h.BuzzBin, "buzz")); err != nil {
		if _, err := exec.LookPath("buzz"); err != nil {
			warn("no 'buzz' CLI found on PATH or at %s", h.BuzzBin)
			warn("  point BUZZ_BIN at an existing Buzz install, or build one:")
			warn("  git clone --depth 1 https://github.com/block/buzz.git %s/buzz", h.FleetHome)
			warn("  cd %s/buzz && cargo build --release -p buzz -p buzz-acp -p buzz-admin", h.FleetHome)
		}
	}

	fmt.Printf("\nNext: buzzfleet init --invite <v2....>\n")
	fmt.Printf("Mint the invite in the Buzz app: right-click your community -> Invite to community.\n")
}

// cmdRunAgent launches one agent harness. The systemd template calls this.
//
// systemd cannot take WorkingDirectory from a config file, and each agent needs
// a different one — the harness passes its own cwd into ACP session/new, which
// is what gives the coding agent its workspace and its per-project memory.
func cmdRunAgent(h *host.Host, args []string) {
	if len(args) == 0 {
		die("usage: buzzfleet run-agent <instance>")
	}
	// The unit instance is "agent" for the community the agent was created
	// in and "agent--community" for each other one.
	instance := args[0]
	hn := fleet.ParseInstance(instance)
	name := hn.Agent
	conf, err := h.AgentConfig(name)
	if err != nil {
		die("%v", err)
	}
	if err := fleet.CheckCommunity(conf.Relay, conf.Communities, hn.Community); err != nil {
		die("%s: %v", instance, err)
	}
	member, ok := conf.In(hn.Community)
	if !ok {
		die("%s has no community called %q in agent.json", name, hn.Community)
	}
	if _, err := os.Stat(conf.Workdir); err != nil {
		die("workdir does not exist: %s", conf.Workdir)
	}

	dir := filepath.Join(h.AgentsDir(), name)
	env := os.Environ()

	// The secret comes from a 0600 file into the environment, never into the
	// unit file or the process arguments — argv is world-readable in `ps`.
	f, err := os.Open(filepath.Join(dir, "identity.env"))
	if err != nil {
		die("cannot read identity for %s: %v", name, err)
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		env = setEnv(env, k, v)
	}
	f.Close()

	agentCmd := conf.AgentCommand
	if agentCmd == "" {
		agentCmd = "claude-agent-acp"
	}
	respondTo := member.RespondTo
	if respondTo == "" {
		respondTo = "owner-only"
	}

	home, _ := os.UserHomeDir()
	// PATH must be *replaced*, not appended. A second PATH= entry in the
	// environment is not merged: the first one wins, so appending silently
	// leaves systemd's minimal path in force and buzz-acp cannot find its
	// runtime adapter. `buzz` must be on it too — that is how the agent posts
	// its replies back.
	env = setEnv(env, "PATH", strings.Join([]string{
		h.BuzzBin, filepath.Join(home, ".npm-global/bin"), os.Getenv("PATH"),
	}, ":"))
	env = setEnv(env, "BUZZ_RELAY_URL", member.Relay)
	// Which harness this is, so doctor and reap can tell an agent's harnesses
	// in different communities apart from a duplicate in the same one.
	env = setEnv(env, "BUZZ_FLEET_HARNESS", instance)
	env = setEnv(env, "BUZZ_FLEET_COMMUNITY", hn.Community)
	env = setEnv(env, "BUZZ_ACP_AGENT_COMMAND", agentCmd)
	// The turn gate: one turn at a time for the agent, across every
	// community it is in. An agent in one community has nothing to share its
	// workdir with, so it runs exactly as before. BUZZ_FLEET_NO_GATE=1 in a
	// drop-in turns the gate off.
	if len(conf.Communities) > 0 && os.Getenv(envNoGate) == "" {
		// Each coding-agent slot would get a gate of its own, and only one
		// gate per harness can run; the others would fail to start.
		if n, _ := strconv.Atoi(os.Getenv("BUZZ_ACP_AGENTS")); n > 1 {
			die("%s: BUZZ_ACP_AGENTS=%d cannot be combined with the turn gate, which runs one turn at a time for an agent in several communities; set it to 1, or set %s=1 to turn the gate off", instance, n, envNoGate)
		}
		gated, genv, err := installGate(h, instance, agentCmd, env)
		if err != nil {
			die("%s: turn gate: %v", instance, err)
		}
		env = setEnv(genv, "BUZZ_ACP_AGENT_COMMAND", gated)
	}
	// Containers are made by dockerd, outside this unit's cgroup, so they are
	// capped on the way in instead: the supervisor serves this socket.
	if cfg, err := loadConfig(h); err == nil && !cfg.Limits.NoDockerProxy {
		env = setEnv(env, "DOCKER_HOST", "unix://"+host.DockerSocketFor(instance))
	}

	argv := []string{"buzz-acp", "--respond-to", respondTo}
	if member.Owner != "" {
		argv = append(argv, "--agent-owner", member.Owner)
	}
	if len(conf.Communities) > 0 {
		// Several communities: the personality, this community's addendum,
		// and what the agent needs to know about being in more than one.
		p, extra, err := harnessPrompt(h, conf, hn)
		if err != nil {
			die("%s: prompt: %v", instance, err)
		}
		for _, kv := range extra {
			k, v, _ := strings.Cut(kv, "=")
			env = setEnv(env, k, v)
		}
		argv = append(argv, "--system-prompt-file", p)
	} else if prompt := filepath.Join(dir, "system-prompt.md"); fileExists(prompt) {
		argv = append(argv, "--system-prompt-file", prompt)
	}

	bin, err := exec.LookPath(filepath.Join(h.BuzzBin, "buzz-acp"))
	if err != nil {
		if bin, err = exec.LookPath("buzz-acp"); err != nil {
			die("buzz-acp not found on PATH or at %s", h.BuzzBin)
		}
	}
	if err := os.Chdir(conf.Workdir); err != nil {
		die("cannot enter %s: %v", conf.Workdir, err)
	}
	// Exec rather than spawn, so systemd supervises the harness directly and
	// there is no shell left in the middle of the control group.
	if err := syscall.Exec(bin, argv, env); err != nil {
		die("exec buzz-acp: %v", err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// setEnv replaces a variable in an environment slice, or appends it if absent.
//
// Appending a duplicate does not override: exec passes the slice through
// verbatim and lookups return the first match, so a second PATH= is ignored.
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
