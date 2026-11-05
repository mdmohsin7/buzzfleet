package host

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

// ReadMetrics takes one reading of the machine from /proc and statfs. Missing
// files leave zeroes rather than failing: a partial heartbeat still proves the
// host is alive, which is its main job.
func ReadMetrics(diskPath string) fleet.HostMetrics {
	var m fleet.HostMetrics
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			m.Load1, _ = strconv.ParseFloat(f[0], 64)
			m.Load5, _ = strconv.ParseFloat(f[1], 64)
			m.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	m.CPUs = runtime.NumCPU()
	if f, err := os.Open("/proc/meminfo"); err == nil {
		s := bufio.NewScanner(f)
		for s.Scan() {
			fs := strings.Fields(s.Text())
			if len(fs) < 2 {
				continue
			}
			kb, _ := strconv.ParseUint(fs[1], 10, 64)
			switch fs[0] {
			case "MemTotal:":
				m.MemTotal = kb << 10
			case "MemAvailable:":
				m.MemAvail = kb << 10
			case "SwapTotal:":
				m.SwapTotal = kb << 10
			case "SwapFree:":
				m.SwapFree = kb << 10
			}
		}
		f.Close()
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &st); err == nil {
		m.DiskTotal = st.Blocks * uint64(st.Bsize)
		m.DiskFree = st.Bavail * uint64(st.Bsize)
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			v, _ := strconv.ParseFloat(f[0], 64)
			m.UptimeSecs = int64(v)
		}
	}
	return m
}
