// Package host inspects and controls this machine: systemd units, agent
// directories, the process tree, and the buzz CLI.
//
// Everything that touches the outside world lives here, so package fleet can
// stay pure and testable.
package host

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Host struct {
	FleetHome string
	BuzzBin   string // directory containing the buzz binary
}

func New(fleetHome string) *Host {
	return &Host{FleetHome: fleetHome, BuzzBin: FindBuzzBin(fleetHome)}
}

// FindBuzzBin locates the buzz CLI: an explicit BUZZ_BIN, then a checkout
// beside us, then PATH. Requiring a 1.4GB clone and a Rust toolchain to obtain
// a CLI you may already have is the main reason setup gets abandoned.
func FindBuzzBin(fleetHome string) string {
	if v := os.Getenv("BUZZ_BIN"); v != "" {
		return v
	}
	local := filepath.Join(fleetHome, "buzz", "target", "release")
	if _, err := os.Stat(filepath.Join(local, "buzz")); err == nil {
		return local
	}
	if p, err := exec.LookPath("buzz"); err == nil {
		return filepath.Dir(p)
	}
	return local
}

func (h *Host) AgentsDir() string { return filepath.Join(h.FleetHome, "agents") }

func (h *Host) env() []string {
	env := os.Environ()
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		env = append(env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()))
	}
	return env
}

// ── systemd ───────────────────────────────────────────────────────────────────

func UnitFor(name string) string { return "buzz-agent@" + name + ".service" }

// Systemctl returns output even on failure: systemctl exits non-zero for
// ordinary states like "inactive", so the text is the answer, not an error.
func (h *Host) Systemctl(args ...string) string {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Env = h.env()
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// Journal returns the last n lines for a unit.
//
// Logs come from journalctl, not `systemctl status`: systemctl has no -u flag
// (that is journalctl's), and its status output is mostly unit metadata with a
// handful of log lines stapled on the end.
func (h *Host) Journal(name string, lines string) string {
	cmd := exec.Command("journalctl", "--user", "-u", UnitFor(name), "-n", lines, "--no-pager")
	cmd.Env = h.env()
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Sprintf("could not read the journal for %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

func (h *Host) UnitState(name string) string { return h.Systemctl("is-active", UnitFor(name)) }
func (h *Host) UnitEnabled(name string) bool {
	return h.Systemctl("is-enabled", UnitFor(name)) == "enabled"
}
func (h *Host) MainPID(name string) string {
	pid := h.Systemctl("show", UnitFor(name), "-p", "MainPID", "--value")
	if pid == "0" {
		return ""
	}
	return pid
}

// UpSince is when the unit last became active, or zero if unknown.
func (h *Host) UpSince(name string) time.Time {
	ts := h.Systemctl("show", UnitFor(name), "-p", "ActiveEnterTimestamp", "--value")
	if ts == "" {
		return time.Time{}
	}
	t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (h *Host) Uptime(name string) string {
	t := h.UpSince(name)
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ── agents ────────────────────────────────────────────────────────────────────

type AgentConfig struct {
	Name         string `json:"name"`
	Workdir      string `json:"workdir"`
	Relay        string `json:"relay"`
	AgentCommand string `json:"agentCommand"`
	RespondTo    string `json:"respondTo"`
	Owner        string `json:"owner"`
}

func (h *Host) Agents() []string {
	entries, err := os.ReadDir(h.AgentsDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(h.AgentsDir(), e.Name(), "agent.json")); err == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func (h *Host) AgentConfig(name string) (*AgentConfig, error) {
	b, err := os.ReadFile(filepath.Join(h.AgentsDir(), name, "agent.json"))
	if err != nil {
		return nil, fmt.Errorf("no such agent: %s", name)
	}
	var c AgentConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: unreadable agent.json: %w", name, err)
	}
	return &c, nil
}

var pubkeyRe = regexp.MustCompile(`(?m)^BUZZ_PUBLIC_KEY=([0-9a-f]{64})`)
var seckeyRe = regexp.MustCompile(`(?m)^BUZZ_PRIVATE_KEY=(\S+)`)

func (h *Host) identityFile(name string) string {
	return filepath.Join(h.AgentsDir(), name, "identity.env")
}

func (h *Host) AgentSecret(name string) string {
	b, err := os.ReadFile(h.identityFile(name))
	if err != nil {
		return ""
	}
	if m := seckeyRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// AgentPubkey prefers the recorded key and derives it otherwise: an agent
// provisioned from a deploy payload is given only its secret.
func (h *Host) AgentPubkey(name string, derive func(string) (string, error)) string {
	b, err := os.ReadFile(h.identityFile(name))
	if err != nil {
		return ""
	}
	if m := pubkeyRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	if m := seckeyRe.FindSubmatch(b); m != nil && derive != nil {
		if pk, err := derive(string(m[1])); err == nil {
			return pk
		}
	}
	return ""
}

func (h *Host) IdentityMode(name string) string {
	fi, err := os.Stat(h.identityFile(name))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%o", fi.Mode().Perm())
}

// ── process tree ──────────────────────────────────────────────────────────────

var runtimeRe = regexp.MustCompile(`(?i)acp|claude|codex|goose`)

// ChildAlive reports whether the harness still has its coding-agent subprocess.
//
// Presence on the relay proves the harness's relay loop is alive; it does not
// prove the agent underneath it is. The ACP child can die while the relay task
// keeps heartbeating, leaving something that looks healthy and answers nothing.
func (h *Host) ChildAlive(name string) (alive bool, known bool) {
	pid := h.MainPID(name)
	if pid == "" {
		return false, false
	}
	out, err := exec.Command("ps", "--ppid", pid, "-o", "args=").Output()
	if err != nil {
		return false, true // no children at all
	}
	return runtimeRe.Match(out), true
}

type Proc struct {
	PID        string
	Agent      string
	Supervised bool
}

var harnessRe = regexp.MustCompile(`^(\d+)\s+(.*buzz-acp\b.*)$`)
var promptRe = regexp.MustCompile(`agents/([^/]+)/system-prompt\.md`)

// Harnesses lists every buzz-acp process, tagged with whether systemd owns it.
// Unsupervised ones are what produce duplicate replies: two harnesses on one
// key both answer every mention, and a restart cannot reach the stray.
func (h *Host) Harnesses() []Proc {
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	supervised := map[string]bool{}
	for _, n := range h.Agents() {
		if pid := h.MainPID(n); pid != "" {
			supervised[pid] = true
		}
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		m := harnessRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		if strings.HasPrefix(m[2], "/bin/bash") || strings.Contains(m[2], "buzz-agent-run") {
			continue
		}
		agent := "unknown"
		if pm := promptRe.FindStringSubmatch(m[2]); pm != nil {
			agent = pm[1]
		}
		procs = append(procs, Proc{PID: m[1], Agent: agent, Supervised: supervised[m[1]]})
	}
	return procs
}

// ── buzz CLI ──────────────────────────────────────────────────────────────────

func HTTPRelay(u string) string {
	u = strings.Replace(u, "wss://", "https://", 1)
	return strings.Replace(u, "ws://", "http://", 1)
}

func WSRelay(u string) string {
	u = strings.Replace(u, "https://", "wss://", 1)
	return strings.Replace(u, "http://", "ws://", 1)
}

// Buzz runs the buzz CLI as a given identity. Shelling out keeps signing and
// NIP-98 on one tested upstream path rather than reimplementing them.
func (h *Host) Buzz(secret, relay string, args ...string) (string, error) {
	cmd := exec.Command(filepath.Join(h.BuzzBin, "buzz"), args...)
	cmd.Env = append(h.env(),
		"BUZZ_PRIVATE_KEY="+secret,
		"BUZZ_RELAY_URL="+HTTPRelay(relay),
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type Channel struct {
	ID          string `json:"channel_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (h *Host) Channels(secret, relay string) ([]Channel, error) {
	out, err := h.Buzz(secret, relay, "channels", "list")
	if err != nil {
		return nil, err
	}
	var cs []Channel
	return cs, json.Unmarshal([]byte(out), &cs)
}

type Member struct {
	Pubkey string `json:"pubkey"`
	Role   string `json:"role"`
}

func (h *Host) Members(secret, relay, channel string) ([]Member, error) {
	out, err := h.Buzz(secret, relay, "channels", "members", "--channel", channel)
	if err != nil {
		return nil, err
	}
	var ms []Member
	return ms, json.Unmarshal([]byte(out), &ms)
}

type Presence struct {
	Pubkey    string `json:"pubkey"`
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updated_at"`
}

// PresenceOf asks for the whole fleet in one call rather than one per agent.
//
// What matters is whether a record exists at all: `updated_at` is synthesised
// at read time, so its age says nothing. buzz-acp republishes presence every
// 60s and the relay expires it after 90s, so a harness that has stopped
// publishing simply disappears.
func (h *Host) PresenceOf(secret, relay string, pubkeys []string) (map[string]bool, error) {
	if len(pubkeys) == 0 {
		return map[string]bool{}, nil
	}
	out, err := h.Buzz(secret, relay, "users", "presence", "--pubkeys", strings.Join(pubkeys, ","))
	if err != nil {
		return nil, err
	}
	var rows []Presence
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	online := map[string]bool{}
	for _, r := range rows {
		if r.Status != "offline" {
			online[r.Pubkey] = true
		}
	}
	return online, nil
}
