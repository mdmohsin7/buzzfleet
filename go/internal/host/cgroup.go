package host

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

const cgroupRoot = "/sys/fs/cgroup"

// CgroupMode reports how control groups are mounted, and which controllers the
// user manager may use. On v1 nothing can be delegated, so the list is empty.
func CgroupMode() (fleet.CgroupMode, []string) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(cgroupRoot, &st); err == nil && st.Type == 0x63677270 { // CGROUP2_SUPER_MAGIC
		uid := os.Getuid()
		b, _ := os.ReadFile(filepath.Join(cgroupRoot, "user.slice",
			"user-"+strconv.Itoa(uid)+".slice", "user@"+strconv.Itoa(uid)+".service", "cgroup.subtree_control"))
		return fleet.CgroupUnified, strings.Fields(string(b))
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "unified")); err == nil {
		return fleet.CgroupHybrid, nil
	}
	return fleet.CgroupLegacy, nil
}

// MemTotalMB is the machine's RAM, from /proc/meminfo.
func MemTotalMB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if f := strings.Fields(s.Text()); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

// NumCPU is the cores this process may run on, which is what the agents get.
func NumCPU() int { return runtime.NumCPU() }

// procCgroups maps controller to cgroup path for a process. The unified
// hierarchy is under the key "".
func procCgroups(pid string) map[string]string {
	b, err := os.ReadFile("/proc/" + pid + "/cgroup")
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[1] == "" {
			out[""] = parts[2]
			continue
		}
		for _, c := range strings.Split(parts[1], ",") {
			out[c] = parts[2]
		}
	}
	return out
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ancestors lists a cgroup path and every parent up to the root.
func ancestors(p string) []string {
	var out []string
	for {
		out = append(out, p)
		if p == "/" || p == "" || p == "." {
			return out
		}
		p = filepath.Dir(p)
	}
}

// Ceilings reads the kernel's limits for a process's cgroup and all of its
// ancestors, and reports whether the process has a memory cgroup of its own —
// without one, a per-agent memory limit has nothing to apply to.
func Ceilings(pid string, mode fleet.CgroupMode) (fleet.Ceiling, bool) {
	cg := procCgroups(pid)
	if cg == nil {
		return fleet.Ceiling{}, false
	}
	var readings []fleet.CgroupReading
	if mode == fleet.CgroupUnified {
		own := false
		for i, p := range ancestors(cg[""]) {
			dir := filepath.Join(cgroupRoot, p)
			r := fleet.CgroupReading{Path: p, CPUMax: readTrim(filepath.Join(dir, "cpu.max")),
				MemoryMax: readTrim(filepath.Join(dir, "memory.max"))}
			if i == 0 && r.MemoryMax != "" {
				own = true
			}
			readings = append(readings, r)
		}
		return fleet.Tightest(readings), own
	}

	cpuDir := firstDir(filepath.Join(cgroupRoot, "cpu,cpuacct"), filepath.Join(cgroupRoot, "cpu"))
	for _, p := range ancestors(cg["cpu"]) {
		q := readTrim(filepath.Join(cpuDir, p, "cpu.cfs_quota_us"))
		per := readTrim(filepath.Join(cpuDir, p, "cpu.cfs_period_us"))
		if q != "" {
			readings = append(readings, fleet.CgroupReading{Path: p, CPUMax: q + " " + per})
		}
	}
	for _, p := range ancestors(cg["memory"]) {
		if v := readTrim(filepath.Join(cgroupRoot, "memory", p, "memory.limit_in_bytes")); v != "" {
			readings = append(readings, fleet.CgroupReading{Path: p, MemoryMax: v})
		}
	}
	// On v1 the user manager cannot create cgroups in the memory hierarchy, so
	// every agent sits in the user manager's group and a per-agent MemoryMax=
	// is accepted and ignored.
	base := filepath.Base(cg["memory"])
	return fleet.Tightest(readings), strings.HasSuffix(base, ".service") && !strings.HasPrefix(base, "user@")
}

func firstDir(paths ...string) string {
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return paths[len(paths)-1]
}
