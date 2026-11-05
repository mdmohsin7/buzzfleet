package fleet

import (
	"strings"
	"testing"
)

// ── sizing ────────────────────────────────────────────────────────────────────

func TestSizeLimitsKeepsHeadroomOnASmallBox(t *testing.T) {
	// 2 cores / 12G: the size of host that hit load 50.
	l := SizeLimits(2, 12000, LimitsConfig{})
	if l.CPUPercent != 150 {
		t.Fatalf("2 cores should leave the host half a core, got %d%%", l.CPUPercent)
	}
	if l.MemoryMaxMB != 12000-1500 || l.MemoryHighMB != l.MemoryMaxMB-1024 {
		t.Fatalf("memory: high=%d max=%d", l.MemoryHighMB, l.MemoryMaxMB)
	}
	if l.DockerCPUs != 1 || l.DockerMemoryMB != 3000 {
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
	l := SizeLimits(2, 12000, LimitsConfig{CPUPercent: 120, MemoryMaxMB: 8000, DockerCPUs: 0.5, DockerMemoryMB: 1024})
	if l.CPUPercent != 120 || l.MemoryMaxMB != 8000 || l.MemoryHighMB != 6976 || l.DockerCPUs != 0.5 || l.DockerMemoryMB != 1024 {
		t.Fatalf("overrides not applied: %+v", l)
	}
	l = SizeLimits(2, 12000, LimitsConfig{MemoryMaxMB: 4000, MemoryHighMB: 9000})
	if l.MemoryHighMB != 4000 {
		t.Fatalf("throttle point must be clamped to the ceiling, got %d", l.MemoryHighMB)
	}
}

// ── where the ceiling goes ────────────────────────────────────────────────────

func TestPlanLimitsUsesUserSliceWhenDelegated(t *testing.T) {
	tg := PlanLimits(CgroupUnified, []string{"cpu", "memory", "pids"}, 1000, SizeLimits(2, 12000, LimitsConfig{}))
	if tg.System || tg.Unit != AgentSlice {
		t.Fatalf("delegated v2 needs no root: %+v", tg)
	}
	if got := tg.String(); got != "systemctl --user set-property buzzfleet.slice CPUQuota=150% MemoryHigh=9476M MemoryMax=10500M" {
		t.Fatalf("command: %s", got)
	}
}

func TestPlanLimitsOnV1CapsTheUserManager(t *testing.T) {
	// An older host: hybrid hierarchy, nothing delegated. A CPUQuota on the
	// user slice would be accepted and silently ignored.
	for _, mode := range []CgroupMode{CgroupHybrid, CgroupLegacy} {
		tg := PlanLimits(mode, nil, 1000, SizeLimits(2, 12000, LimitsConfig{}))
		if !tg.System || tg.Unit != "user@1000.service" {
			t.Fatalf("%s: must go on the system unit: %+v", mode, tg)
		}
		got := tg.String()
		if !strings.HasPrefix(got, "sudo systemctl set-property user@1000.service ") ||
			!strings.Contains(got, "CPUQuota=150%") || !strings.Contains(got, "MemoryLimit=10500M") {
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
		{Path: "/user.slice/user-1000.slice/user@1000.service/buzzfleet.slice/buzz-agent@agent-a.service", CPUMax: "max 100000", MemoryMax: "max"},
		{Path: "/user.slice/user-1000.slice/user@1000.service/buzzfleet.slice", CPUMax: "150000 100000", MemoryMax: "11010048000"},
		{Path: "/user.slice", CPUMax: "max 100000", MemoryMax: "max"},
	})
	if c.CPUPercent != 150 || c.MemoryMB != 10500 || !strings.HasSuffix(c.CPUAt, "buzzfleet.slice") {
		t.Fatalf("got %+v", c)
	}
	if got := c.String(); got != "CPU 150% (on buzzfleet.slice), memory 10500M (on buzzfleet.slice)" {
		t.Fatalf("got %q", got)
	}
}

func TestTightestV1UnlimitedIsNoCeiling(t *testing.T) {
	// What an unconfigured v1 host reports: quota -1 and a page-rounded int64 max.
	c := Tightest([]CgroupReading{
		{Path: "/user.slice", CPUMax: "-1 100000"},
		{Path: "/user.slice/user-1000.slice/user@1000.service", MemoryMax: "9223372036854771712"},
	})
	if c.CPUAt != "" || c.MemoryAt != "" {
		t.Fatalf("unlimited must not read as a ceiling: %+v", c)
	}
	if got := c.String(); got != "CPU: NO ceiling, memory: NO ceiling" {
		t.Fatalf("got %q", got)
	}
}

func TestSmallHostIsNotLeftReclaiming(t *testing.T) {
	l := SizeLimits(1, 2048, LimitsConfig{})
	if l.MemoryMaxMB != 1024 || l.MemoryHighMB != 768 {
		t.Fatalf("throttle at 3/4 of the ceiling on a small host, got high=%d max=%d", l.MemoryHighMB, l.MemoryMaxMB)
	}
}

func TestPlanLimitsNeedsBothControllers(t *testing.T) {
	// cpu alone is not enough: the memory ceiling would be accepted and ignored.
	tg := PlanLimits(CgroupUnified, []string{"cpu", "pids"}, 1000, SizeLimits(2, 12000, LimitsConfig{}))
	if !tg.System {
		t.Fatalf("without memory delegated the ceiling must go on the user manager: %+v", tg)
	}
}

func TestTightestPicksTheLowestMemory(t *testing.T) {
	c := Tightest([]CgroupReading{
		{Path: "/a/b/c", MemoryMax: "4294967296"}, // 4096M
		{Path: "/a/b", MemoryMax: "2147483648"},   // 2048M: this one binds
		{Path: "/a", MemoryMax: "8589934592"},
	})
	if c.MemoryMB != 2048 || c.MemoryAt != "/a/b" {
		t.Fatalf("got %+v", c)
	}
}
