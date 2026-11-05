package fleet

import (
	"fmt"
	"strings"
)

// ── resource limits ───────────────────────────────────────────────────────────
//
// A 2-core host running a handful of coding agents is one parallel test suite
// away from a load of 50, at which point sshd, the supervisor and every other
// agent crawl together. Nice alone does not stop that: it orders who runs
// first, but a box with every core saturated is still saturated. The fix is a
// hard ceiling on everything the agents can use, sized so the host always keeps
// a slice of CPU and memory for itself.

// LimitsConfig is the operator's override. Zero fields are sized from the host.
type LimitsConfig struct {
	CPUPercent   int `json:"cpuPercent,omitempty"`   // all agents together; 100 = one core
	MemoryHighMB int `json:"memoryHighMB,omitempty"` // throttle point (cgroup v2 only)
	MemoryMaxMB  int `json:"memoryMaxMB,omitempty"`  // hard ceiling: past this, the kernel kills

	// Per container started through the Docker proxy.
	DockerCPUs     float64 `json:"dockerCpus,omitempty"`
	DockerMemoryMB int     `json:"dockerMemoryMB,omitempty"`
	NoDockerProxy  bool    `json:"noDockerProxy,omitempty"`
}

// Limits is what will actually be applied.
type Limits struct {
	CPUPercent     int
	MemoryHighMB   int
	MemoryMaxMB    int
	DockerCPUs     float64
	DockerMemoryMB int
}

// SizeLimits works out the ceilings for a host from its core count and RAM.
//
// Half a core is kept back for the host: enough for sshd and the supervisor to
// stay responsive, which is all that is needed to fix whatever went wrong. On a
// 2-core box that leaves the agents 150%. Memory keeps back an eighth of RAM or
// 1 GiB, whichever is larger, below the hard ceiling, and another GiB below the
// throttle point so the kernel starts reclaiming before anything is killed.
func SizeLimits(nproc, ramMB int, o LimitsConfig) Limits {
	if nproc < 1 {
		nproc = 1
	}
	l := Limits{
		CPUPercent:     max(50, nproc*100-50),
		DockerCPUs:     min(1, float64(nproc)/2),
		DockerMemoryMB: max(256, min(3072, ramMB/4)),
	}
	reserve := max(1024, ramMB/8)
	l.MemoryMaxMB = max(256, ramMB-reserve)
	l.MemoryHighMB = max(256, l.MemoryMaxMB-1024)

	if o.CPUPercent > 0 {
		l.CPUPercent = o.CPUPercent
	}
	if o.MemoryMaxMB > 0 {
		l.MemoryMaxMB = o.MemoryMaxMB
		if o.MemoryHighMB == 0 {
			l.MemoryHighMB = max(256, o.MemoryMaxMB-1024)
		}
	}
	if o.MemoryHighMB > 0 {
		l.MemoryHighMB = o.MemoryHighMB
	}
	// A throttle point above the ceiling is never reached; clamp rather than
	// write a setting that silently does nothing.
	l.MemoryHighMB = min(l.MemoryHighMB, l.MemoryMaxMB)
	if o.DockerCPUs > 0 {
		l.DockerCPUs = o.DockerCPUs
	}
	if o.DockerMemoryMB > 0 {
		l.DockerMemoryMB = o.DockerMemoryMB
	}
	return l
}

// CgroupMode is how the host's control groups are mounted.
type CgroupMode string

const (
	CgroupUnified CgroupMode = "v2"     // one hierarchy; systemd can delegate controllers
	CgroupHybrid  CgroupMode = "hybrid" // v1 controllers, v2 only for process tracking
	CgroupLegacy  CgroupMode = "v1"
)

// LimitTarget is where the fleet-wide ceiling is applied, and how.
type LimitTarget struct {
	System bool     // a system unit: needs root
	Unit   string   // the unit carrying the ceiling
	Props  []string // systemctl set-property arguments
	Why    string
}

// Command is the exact command that applies the target, for showing before
// running and for running.
func (t LimitTarget) Command() []string {
	argv := []string{"systemctl"}
	if !t.System {
		argv = append(argv, "--user")
	}
	argv = append(argv, "set-property", t.Unit)
	return append(argv, t.Props...)
}

func (t LimitTarget) String() string {
	s := strings.Join(t.Command(), " ")
	if t.System {
		s = "sudo " + s
	}
	return s
}

// PlanLimits picks where the ceiling goes.
//
// The natural home is buzz-agent.slice in the user manager: no root needed, and
// it holds exactly the agents. That only works when the user manager has been
// given the cpu and memory controllers, which on cgroup v2 is common and on v1
// is impossible — systemd silently accepts a CPUQuota it cannot apply. There the
// ceiling goes one level up, on the system unit user@UID.service. That cgroup
// holds every user unit, agents and supervisor, but not SSH sessions, which are
// session scopes beside it, so a saturated fleet still cannot lock you out.
func PlanLimits(mode CgroupMode, delegated []string, uid int, l Limits) LimitTarget {
	has := func(c string) bool {
		for _, d := range delegated {
			if d == c {
				return true
			}
		}
		return false
	}
	cpu := fmt.Sprintf("CPUQuota=%d%%", l.CPUPercent)
	if mode == CgroupUnified && has("cpu") && has("memory") {
		return LimitTarget{
			Unit: "buzz-agent.slice",
			Props: []string{cpu,
				fmt.Sprintf("MemoryHigh=%dM", l.MemoryHighMB),
				fmt.Sprintf("MemoryMax=%dM", l.MemoryMaxMB)},
			Why: "cgroup v2 with cpu and memory delegated to the user manager: the ceiling covers exactly the agents",
		}
	}
	t := LimitTarget{System: true, Unit: fmt.Sprintf("user@%d.service", uid)}
	if mode == CgroupUnified {
		t.Props = []string{cpu,
			fmt.Sprintf("MemoryHigh=%dM", l.MemoryHighMB),
			fmt.Sprintf("MemoryMax=%dM", l.MemoryMaxMB)}
		t.Why = "cgroup v2, but the user manager was not given cpu+memory: the ceiling goes on the user manager itself (agents and supervisor, not SSH sessions)"
		return t
	}
	// cgroup v1 has no throttle point, and systemd versions of this vintage
	// apply the hard ceiling to memory.limit_in_bytes through MemoryLimit=.
	t.Props = []string{"CPUAccounting=yes", "MemoryAccounting=yes", cpu,
		fmt.Sprintf("MemoryLimit=%dM", l.MemoryMaxMB)}
	t.Why = "cgroup v1: controllers cannot be delegated to the user manager, so the ceiling goes on the user manager itself (agents and supervisor, not SSH sessions)"
	return t
}

// ── what the kernel is actually enforcing ─────────────────────────────────────
//
// systemd accepts limits it cannot apply, and says nothing. The only reliable
// answer to "is this agent capped?" is the kernel's own files, read for every
// cgroup from the agent's up to the root: a ceiling anywhere above it counts.

// CgroupReading is what was found in the kernel's files for one cgroup. Empty
// strings mean the file does not exist: that controller is not applied there.
type CgroupReading struct {
	Path      string
	CPUMax    string // v2 "cpu.max", or v1 "<cfs_quota_us> <cfs_period_us>"
	MemoryMax string // v2 "memory.max", or v1 memory.limit_in_bytes
}

// ParseCPUMax turns "150000 100000" into 150 (percent of one core).
func ParseCPUMax(s string) (float64, bool) {
	f := strings.Fields(s)
	if len(f) != 2 || f[0] == "max" || f[0] == "-1" {
		return 0, false
	}
	var q, p float64
	if _, err := fmt.Sscan(f[0], &q); err != nil || q <= 0 {
		return 0, false
	}
	if _, err := fmt.Sscan(f[1], &p); err != nil || p <= 0 {
		return 0, false
	}
	return q / p * 100, true
}

// ParseMemoryMax turns a byte count into MiB. v1 reports "unlimited" as a
// page-rounded int64 max, v2 as "max".
func ParseMemoryMax(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" {
		return 0, false
	}
	var b float64
	if _, err := fmt.Sscan(s, &b); err != nil || b <= 0 || b >= 1<<62 {
		return 0, false
	}
	return int64(b) >> 20, true
}

// Ceiling is the tightest limit found, and the cgroup it is set on.
type Ceiling struct {
	CPUPercent float64
	CPUAt      string
	MemoryMB   int64
	MemoryAt   string
}

// Tightest finds the lowest CPU and memory ceiling among a cgroup and its
// ancestors — the one that actually binds.
func Tightest(readings []CgroupReading) Ceiling {
	var c Ceiling
	for _, r := range readings {
		if pct, ok := ParseCPUMax(r.CPUMax); ok && (c.CPUAt == "" || pct < c.CPUPercent) {
			c.CPUPercent, c.CPUAt = pct, r.Path
		}
		if mb, ok := ParseMemoryMax(r.MemoryMax); ok && (c.MemoryAt == "" || mb < c.MemoryMB) {
			c.MemoryMB, c.MemoryAt = mb, r.Path
		}
	}
	return c
}

func (c Ceiling) String() string {
	cpu := "CPU: NO ceiling"
	if c.CPUAt != "" {
		cpu = fmt.Sprintf("CPU %.0f%% (on %s)", c.CPUPercent, lastSegment(c.CPUAt))
	}
	mem := "memory: NO ceiling"
	if c.MemoryAt != "" {
		mem = fmt.Sprintf("memory %dM (on %s)", c.MemoryMB, lastSegment(c.MemoryAt))
	}
	return cpu + ", " + mem
}

func lastSegment(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	if p == "" {
		return "/"
	}
	return p
}
