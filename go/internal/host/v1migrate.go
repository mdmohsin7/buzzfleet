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
		`f=$1; shift; for p; do echo "$p" > "$f" 2>/dev/null || true; done`, "sh", target}, pids...)
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
