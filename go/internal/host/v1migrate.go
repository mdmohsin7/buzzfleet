package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// On cgroup v1, `systemctl set-property` on a running unit creates the unit's
// cgroup in a newly enabled controller hierarchy (cpu, say) and sets the limit
// there, but leaves the unit's running processes where they were. The limit is
// accepted, reported as set, and caps nothing until the unit next restarts —
// which for user@UID.service means stopping every agent at once.
//
// V1Placement finds those processes, and V1Migrate moves them, using the
// "name=systemd" hierarchy as the record of which processes belong to the unit.
// It is where systemd itself would put them at unit start, so nothing drifts
// back. The user manager moves too, so whatever it starts later inherits the cap.

// V1Controllers are the hierarchies a fleet ceiling lives in on cgroup v1.
var V1Controllers = []string{"cpu", "memory"}

// controllerDir is the mount of a v1 controller under root: cpu is usually
// co-mounted as "cpu,cpuacct".
func controllerDir(root, controller string) string {
	if controller == "cpu" {
		return firstDir(filepath.Join(root, "cpu,cpuacct"), filepath.Join(root, "cpu"))
	}
	return filepath.Join(root, controller)
}

// UnitPids lists every process in a unit's subtree of the systemd hierarchy.
func UnitPids(root, unitPath string) ([]string, error) {
	base := filepath.Join(root, "systemd", unitPath)
	if _, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("unit cgroup %s not found: %w", base, err)
	}
	var pids []string
	err := filepath.Walk(base, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Name() != "cgroup.procs" {
			return nil
		}
		b, _ := os.ReadFile(p)
		pids = append(pids, strings.Fields(string(b))...)
		return nil
	})
	return pids, err
}

// V1Misplaced returns, per controller, the unit's live processes that are not
// in the unit's own cgroup of that controller — the ones a limit on the unit
// does not reach.
func V1Misplaced(root, procRoot, unitPath string) (map[string][]string, error) {
	pids, err := UnitPids(root, unitPath)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, pid := range pids {
		b, err := os.ReadFile(filepath.Join(procRoot, pid, "cgroup"))
		if err != nil {
			continue // exited
		}
		for _, c := range V1Controllers {
			if !inController(string(b), c, unitPath) {
				out[c] = append(out[c], pid)
			}
		}
	}
	return out, nil
}

// inController reports whether a /proc/<pid>/cgroup body places the process at
// or below unitPath in the given controller's hierarchy.
func inController(procCgroup, controller, unitPath string) bool {
	for _, line := range strings.Split(strings.TrimSpace(procCgroup), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, c := range strings.Split(parts[1], ",") {
			if c == controller {
				return parts[2] == unitPath || strings.HasPrefix(parts[2], unitPath+"/")
			}
		}
	}
	return false
}

// V1MigrateCommand is the root command that moves pids into a controller's
// cgroup for the unit, one pid per write as the kernel requires. A pid that
// exits between listing and moving fails its write; that is expected and
// skipped, and the verification after it is what decides success.
func V1MigrateCommand(root, controller, unitPath string, pids []string) []string {
	target := filepath.Join(controllerDir(root, controller), unitPath, "cgroup.procs")
	return append([]string{"sudo", "-n", "sh", "-c",
		`f=$1; shift; for p; do echo "$p" 2>/dev/null > "$f" || true; done`, "sh", target}, pids...)
}

// V1TargetExists reports whether systemd created the unit's cgroup in a
// controller hierarchy, which it does when a limit there is set.
func V1TargetExists(root, controller, unitPath string) bool {
	fi, err := os.Stat(filepath.Join(controllerDir(root, controller), unitPath))
	return err == nil && fi.IsDir()
}

// UserManagerPath is user@UID.service's cgroup path.
func UserManagerPath(uid int) string {
	return fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service", uid, uid)
}

// CgroupRoot and ProcRoot are the real mounts; tests pass their own.
const (
	CgroupRoot = cgroupRoot
	ProcRoot   = "/proc"
)

// PlaceV1 moves a unit's running processes into its cgroups in every v1
// controller and verifies the result. It errors, rather than reporting success,
// whenever it cannot show that every process is placed: an empty process list,
// a user manager missing from its own unit or left outside, a controller with
// no cgroup to move into, or any process still outside after two passes.
//
// managerPid is the user manager itself: where it sits decides where everything
// it starts later lands. run executes a command (sudo, in production).
func PlaceV1(root, procRoot, unit, managerPid string, run func(argv []string) error) (map[string]int, error) {
	moved := map[string]int{}
	for pass := 0; pass < 2; pass++ {
		pids, err := UnitPids(root, unit)
		if err != nil {
			return moved, fmt.Errorf("cannot list the processes of %s: %w", unit, err)
		}
		if len(pids) == 0 {
			return moved, fmt.Errorf("found no processes in %s under the systemd hierarchy, so their placement cannot be checked; the ceiling may cover nothing", unit)
		}
		if managerPid != "" && !contains(pids, managerPid) {
			return moved, fmt.Errorf("the user manager (pid %s) is not among the processes of %s; this host's cgroup layout is not what buzzfleet expects, so the ceiling may cover nothing", managerPid, unit)
		}
		mis, err := V1Misplaced(root, procRoot, unit)
		if err != nil {
			return moved, err
		}
		for _, c := range V1Controllers {
			if len(mis[c]) == 0 {
				continue
			}
			if !V1TargetExists(root, c, unit) {
				return moved, fmt.Errorf("the %s limit is set, but systemd made no %s cgroup for %s, so %d running process(es) are not under it",
					c, c, unit, len(mis[c]))
			}
			if err := run(V1MigrateCommand(root, c, unit, mis[c])); err != nil {
				return moved, fmt.Errorf("moving processes into the %s cgroup failed: %w", c, err)
			}
			moved[c] += len(mis[c])
		}
	}
	mis, err := V1Misplaced(root, procRoot, unit)
	if err != nil {
		return moved, fmt.Errorf("cannot verify placement: %w", err)
	}
	for _, c := range V1Controllers {
		if n := len(mis[c]); n > 0 {
			return moved, fmt.Errorf("%d process(es) of %s are still outside its %s cgroup (pids %s), so the ceiling does NOT cover them",
				n, unit, c, strings.Join(mis[c], " "))
		}
	}
	if managerPid != "" {
		b, err := os.ReadFile(filepath.Join(procRoot, managerPid, "cgroup"))
		if err != nil {
			return moved, fmt.Errorf("cannot read the user manager's cgroups: %w", err)
		}
		for _, c := range V1Controllers {
			if !inController(string(b), c, unit) {
				return moved, fmt.Errorf("the user manager (pid %s) is outside its %s cgroup, so agents it starts later will not be capped", managerPid, c)
			}
		}
	}
	return moved, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
