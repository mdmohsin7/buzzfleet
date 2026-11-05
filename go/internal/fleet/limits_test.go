package fleet

import (
	"strings"
	"testing"
)

// ── sizing ────────────────────────────────────────────────────────────────────

func TestSizeLimitsKeepsHeadroomOnTheCylionBox(t *testing.T) {
	// 2 vCPU / 11941M is the production host that hit load 50.
	l := SizeLimits(2, 11941, LimitsConfig{})
	if l.CPUPercent != 150 {
		t.Fatalf("2 cores should leave the host half a core, got %d%%", l.CPUPercent)
	}
	if l.MemoryMaxMB != 11941-1492 || l.MemoryHighMB != l.MemoryMaxMB-1024 {
		t.Fatalf("memory: high=%d max=%d", l.MemoryHighMB, l.MemoryMaxMB)
	}
	if l.DockerCPUs != 1 || l.DockerMemoryMB != 2985 {
		t.Fatalf("docker: %.2f cpus %dM", l.DockerCPUs, l.DockerMemoryMB)
	}
}

func TestSizeLimitsNeverStarvesATinyHost(t *testing.T) {
	l := SizeLimits(1, 1024, LimitsConfig{})
	if l.CPUPercent < 50 || l.MemoryMaxMB < 256 || l.MemoryHighMB < 256 || l.DockerCPUs <= 0 {
		t.Fatalf("a 1-core 1G host still needs room to run one agent: %+v", l)
	}
	if l.MemoryHighMB > l.MemoryMaxMB {
		t.Fatalf("throttle point above the ceiling is never reached: %+v", l)
	}
}

func TestSizeLimitsOverrides(t *testing.T) {
	l := SizeLimits(2, 11941, LimitsConfig{CPUPercent: 120, MemoryMaxMB: 8000, DockerCPUs: 0.5, DockerMemoryMB: 1024})
	if l.CPUPercent != 120 || l.MemoryMaxMB != 8000 || l.MemoryHighMB != 6976 || l.DockerCPUs != 0.5 || l.DockerMemoryMB != 1024 {
		t.Fatalf("overrides not applied: %+v", l)
	}
	l = SizeLimits(2, 11941, LimitsConfig{MemoryMaxMB: 4000, MemoryHighMB: 9000})
	if l.MemoryHighMB != 4000 {
		t.Fatalf("throttle point must be clamped to the ceiling, got %d", l.MemoryHighMB)
	}
}

// ── where the ceiling goes ────────────────────────────────────────────────────

func TestPlanLimitsUsesUserSliceWhenDelegated(t *testing.T) {
	tg := PlanLimits(CgroupUnified, []string{"cpu", "memory", "pids"}, 1001, SizeLimits(2, 11941, LimitsConfig{}))
	if tg.System || tg.Unit != "buzz-agent.slice" {
		t.Fatalf("delegated v2 needs no root: %+v", tg)
	}
	if got := tg.String(); got != "systemctl --user set-property buzz-agent.slice CPUQuota=150% MemoryHigh=9425M MemoryMax=10449M" {
		t.Fatalf("command: %s", got)
	}
}

func TestPlanLimitsOnV1CapsTheUserManager(t *testing.T) {
	// The Cylion box: hybrid hierarchy, nothing delegated. A CPUQuota on the
	// user slice would be accepted and silently ignored.
	for _, mode := range []CgroupMode{CgroupHybrid, CgroupLegacy} {
		tg := PlanLimits(mode, nil, 1001, SizeLimits(2, 11941, LimitsConfig{}))
		if !tg.System || tg.Unit != "user@1001.service" {
			t.Fatalf("%s: must go on the system unit: %+v", mode, tg)
		}
		got := tg.String()
		if !strings.HasPrefix(got, "sudo systemctl set-property user@1001.service ") ||
			!strings.Contains(got, "CPUQuota=150%") || !strings.Contains(got, "MemoryLimit=10449M") {
			t.Fatalf("%s: %s", mode, got)
		}
		if strings.Contains(got, "MemoryHigh") {
			t.Fatalf("%s: v1 has no MemoryHigh, writing it would be a no-op: %s", mode, got)
		}
	}
}

func TestPlanLimitsV2WithoutCPUFallsBackToSystem(t *testing.T) {
	tg := PlanLimits(CgroupUnified, []string{"memory", "pids"}, 1000, SizeLimits(4, 16000, LimitsConfig{}))
	if !tg.System || tg.Unit != "user@1000.service" || !strings.Contains(tg.String(), "MemoryMax=") {
		t.Fatalf("got %+v", tg)
	}
}

// ── reading the kernel ────────────────────────────────────────────────────────

func TestTightestFindsTheBindingAncestor(t *testing.T) {
	c := Tightest([]CgroupReading{
		{Path: "/user.slice/user-1001.slice/user@1001.service/buzz-agent.slice/buzz-agent@dexy.service", CPUMax: "max 100000", MemoryMax: "max"},
		{Path: "/user.slice/user-1001.slice/user@1001.service/buzz-agent.slice", CPUMax: "150000 100000", MemoryMax: "10956570624"},
		{Path: "/user.slice", CPUMax: "max 100000", MemoryMax: "max"},
	})
	if c.CPUPercent != 150 || c.MemoryMB != 10449 || !strings.HasSuffix(c.CPUAt, "buzz-agent.slice") {
		t.Fatalf("got %+v", c)
	}
	if got := c.String(); got != "CPU 150% (on buzz-agent.slice), memory 10449M (on buzz-agent.slice)" {
		t.Fatalf("got %q", got)
	}
}

func TestTightestV1UnlimitedIsNoCeiling(t *testing.T) {
	// What the Cylion box reports today: quota -1 and a page-rounded int64 max.
	c := Tightest([]CgroupReading{
		{Path: "/user.slice", CPUMax: "-1 100000"},
		{Path: "/user.slice/user-1001.slice/user@1001.service", MemoryMax: "9223372036854771712"},
	})
	if c.CPUAt != "" || c.MemoryAt != "" {
		t.Fatalf("unlimited must not read as a ceiling: %+v", c)
	}
	if got := c.String(); got != "CPU: NO ceiling, memory: NO ceiling" {
		t.Fatalf("got %q", got)
	}
}
