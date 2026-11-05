package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// limitsConfig is the operator's override from fleetd.json, or nothing: the
// ceilings are worth having on a host where the supervisor is not set up yet.
func limitsConfig(h *host.Host) fleet.LimitsConfig {
	if c, err := loadConfig(h); err == nil {
		return c.Limits
	}
	return fleet.LimitsConfig{}
}

func hostLimits(h *host.Host) fleet.Limits {
	return fleet.SizeLimits(host.NumCPU(), host.MemTotalMB(), limitsConfig(h))
}

// cmdLimits shows the ceiling this host should have, and with --apply sets it.
func cmdLimits(h *host.Host, args []string) {
	apply := false
	for _, a := range args {
		if a == "--apply" {
			apply = true
		}
	}
	mode, delegated := host.CgroupMode()
	l := hostLimits(h)
	t := fleet.PlanLimits(mode, delegated, os.Getuid(), l)

	fmt.Printf("host        %d cores, %dM RAM, cgroup %s", host.NumCPU(), host.MemTotalMB(), mode)
	if mode == fleet.CgroupUnified {
		fmt.Printf(" (user manager has: %s)", strings.Join(delegated, " "))
	}
	fmt.Printf("\nagents      %d%% CPU and %dM memory between them", l.CPUPercent, l.MemoryMaxMB)
	if mode == fleet.CgroupUnified {
		fmt.Printf(", throttled from %dM", l.MemoryHighMB)
	}
	fmt.Println()
	if limitsConfig(h).NoDockerProxy {
		fmt.Println("containers  not capped (noDockerProxy is set)")
	} else {
		fmt.Printf("containers  %.2g CPU and %dM memory each, via the supervisor's Docker proxy\n", l.DockerCPUs, l.DockerMemoryMB)
	}
	fmt.Printf("\n%s.\n\n  %s\n", t.Why, t)

	if !apply {
		for _, f := range enforcementLines(h, mode, t) {
			if !f.ok && !f.warn && strings.Contains(f.msg, "fleet ceiling does not cover") {
				fmt.Println("\n  ! " + f.msg)
			}
		}
		fmt.Println("\nRun with --apply to set it. It takes effect at once, and persists.")
		return
	}
	argv := t.Command()
	if t.System {
		// -n: fail rather than prompt, so a missing sudo right is reported
		// instead of hanging a non-interactive session.
		argv = append([]string{"sudo", "-n"}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		die("%s failed: %v\n%s", t, err, strings.TrimSpace(string(out)))
	}
	if t.System && mode != fleet.CgroupUnified {
		if err := placeV1(os.Getuid()); err != nil {
			die("%v", err)
		}
	}
	fmt.Println("\napplied. What the kernel now enforces:")
	for _, line := range enforcementLines(h, mode, t) {
		fmt.Println("  " + line.msg)
	}
}

type finding struct {
	ok   bool
	warn bool
	msg  string
}

// enforcementLines says, per running agent, which ceilings the kernel is really
// applying — not what the unit files claim.
func enforcementLines(h *host.Host, mode fleet.CgroupMode, t fleet.LimitTarget) []finding {
	var out []finding
	for _, n := range h.Agents() {
		pid := h.MainPID(n)
		if pid == "" {
			continue
		}
		// A ceiling on the agents' slice only covers agents that are in it.
		// Units written before the template named its slice are not.
		if !t.System {
			if sl := h.Systemctl("show", host.UnitFor(n), "-p", "Slice", "--value"); sl != t.Unit {
				out = append(out, finding{msg: fmt.Sprintf(
					"%s: runs in %s, not %s, so the fleet ceiling does not cover it — run `buzzfleet install --force`, then restart it",
					n, sl, t.Unit)})
			}
		}
		c, own := host.Ceilings(pid, mode)
		out = append(out, finding{ok: c.CPUAt != "" && c.MemoryAt != "",
			msg: fmt.Sprintf("%s: enforced %s", n, c)})

		if v := h.Systemctl("show", host.UnitFor(n), "-p", "MemoryMax", "--value"); !own && v != "" && v != "infinity" {
			out = append(out, finding{warn: true, msg: fmt.Sprintf(
				"%s: its unit sets MemoryMax=%sM but that is NOT enforced — on this host an agent has no memory cgroup of its own",
				n, mib(v))})
		}
	}
	return out
}

func mib(bytes string) string {
	var b int64
	if _, err := fmt.Sscan(bytes, &b); err != nil {
		return bytes
	}
	return fmt.Sprint(b >> 20)
}

// placeV1 moves the user manager's running processes into the cgroups the new
// limit lives in, and fails loudly if any are left outside. On v1, systemd sets
// the limit on the unit's cgroup but leaves running processes where they were,
// so without this `limits --apply` reports success and caps nothing.
func placeV1(uid int) error {
	unit := host.UserManagerPath(uid)
	// Two passes: a process forked during the first is caught by the second.
	moved := map[string]int{}
	for pass := 0; pass < 2; pass++ {
		mis, err := host.V1Misplaced(host.CgroupRoot, host.ProcRoot, unit)
		if err != nil {
			return fmt.Errorf("cannot list the user manager's processes: %w", err)
		}
		for _, c := range host.V1Controllers {
			if len(mis[c]) == 0 {
				continue
			}
			if !host.V1TargetExists(host.CgroupRoot, c, unit) {
				return fmt.Errorf("the %s limit is set, but systemd made no %s cgroup for %s, so %d running process(es) are not under it.\n"+
					"The limit will apply after `sudo systemctl restart user@%d.service`, which stops every agent at once.",
					c, c, unit, len(mis[c]), uid)
			}
			argv := host.V1MigrateCommand(host.CgroupRoot, c, unit, mis[c])
			if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
				return fmt.Errorf("moving processes into the %s cgroup failed: %v\n%s", c, err, strings.TrimSpace(string(out)))
			}
			moved[c] += len(mis[c])
		}
	}
	mis, err := host.V1Misplaced(host.CgroupRoot, host.ProcRoot, unit)
	if err != nil {
		return fmt.Errorf("cannot verify placement: %w", err)
	}
	for _, c := range host.V1Controllers {
		if n := len(mis[c]); n > 0 {
			return fmt.Errorf("the limit is set, but %d process(es) of %s are still outside its %s cgroup (pids %s), so the ceiling does NOT cover them.\n"+
				"Run `buzzfleet limits --apply` again, or `sudo systemctl restart user@%d.service`, which stops every agent at once.",
				n, unit, c, strings.Join(mis[c], " "), uid)
		}
		if moved[c] > 0 {
			fmt.Printf("moved %d running process(es) into the %s cgroup the limit lives in\n", moved[c], c)
		}
	}
	return nil
}
