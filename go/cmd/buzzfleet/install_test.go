package main

import (
	"strings"
	"testing"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

// The ceiling goes on a slice by name; the agents have to be in that slice,
// or `limits --apply` succeeds and caps nothing.
func TestAgentUnitIsInTheSliceTheCeilingGoesOn(t *testing.T) {
	unit := agentUnitBody("/fleet", "/bin/buzzfleet")
	target := fleet.PlanLimits(fleet.CgroupUnified, []string{"cpu", "memory", "pids"}, 1000, fleet.SizeLimits(2, 12000, fleet.LimitsConfig{}))
	if target.System || !strings.Contains(unit, "\nSlice="+target.Unit+"\n") {
		t.Fatalf("ceiling on %s, unit:\n%s", target.Unit, unit)
	}
	if strings.Contains(unit, "%!") {
		t.Fatalf("template formatting broken:\n%s", unit)
	}
}
