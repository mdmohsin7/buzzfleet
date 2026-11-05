package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const unit = "/user.slice/user-1000.slice/user@1000.service"

func put(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeV1 lays out a hybrid host the way systemd 245 leaves it after
// set-property CPUQuota on the user manager: the unit's memory cgroup holds its
// processes, the new cpu cgroup exists but is empty, and the processes still
// sit in cpu:/user.slice.
func fakeV1(t *testing.T) (root, proc string) {
	root, proc = t.TempDir(), t.TempDir()
	put(t, filepath.Join(root, "systemd", unit, "init.scope", "cgroup.procs"), "100\n")
	put(t, filepath.Join(root, "systemd", unit, "buzzfleet.slice", "buzz-agent@agent-a.service", "cgroup.procs"), "200\n201\n")
	put(t, filepath.Join(root, "systemd", unit, "buzzfleetd.service", "cgroup.procs"), "300\n")
	put(t, filepath.Join(root, "cpu,cpuacct", unit, "cgroup.procs"), "")
	for _, pid := range []string{"100", "200", "201"} {
		put(t, filepath.Join(proc, pid, "cgroup"), "12:memory:"+unit+"\n9:cpu,cpuacct:/user.slice\n1:name=systemd:"+unit+"/x\n")
	}
	// Already placed in both.
	put(t, filepath.Join(proc, "300", "cgroup"), "12:memory:"+unit+"\n9:cpu,cpuacct:"+unit+"\n")
	return root, proc
}

func TestV1MisplacedFindsProcessesTheLimitMisses(t *testing.T) {
	root, proc := fakeV1(t)
	mis, err := V1Misplaced(root, proc, unit)
	if err != nil {
		t.Fatal(err)
	}
	got := mis["cpu"]
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"100", "200", "201"}) {
		t.Fatalf("cpu: %v", got)
	}
	if len(mis["memory"]) != 0 {
		t.Fatalf("memory is already placed: %v", mis["memory"])
	}
	// An exited process is not misplaced, it is gone.
	os.RemoveAll(filepath.Join(proc, "201"))
	mis, _ = V1Misplaced(root, proc, unit)
	if len(mis["cpu"]) != 2 {
		t.Fatalf("exited pid counted: %v", mis["cpu"])
	}
}

func TestInControllerMatchesTheUnitSubtreeOnly(t *testing.T) {
	body := "9:cpu,cpuacct:" + unit + "/buzzfleet.slice\n12:memory:/user.slice\n"
	if !inController(body, "cpu", unit) {
		t.Fatal("below the unit counts")
	}
	if inController(body, "memory", unit) {
		t.Fatal("/user.slice is outside the unit")
	}
	if inController("9:cpu,cpuacct:"+unit+"x\n", "cpu", unit) {
		t.Fatal("a sibling with a shared prefix is not inside")
	}
	if inController("0::"+unit+"\n", "cpu", unit) {
		t.Fatal("no cpu line means not placed")
	}
}

func TestV1TargetAndCommand(t *testing.T) {
	root, _ := fakeV1(t)
	if !V1TargetExists(root, "cpu", unit) {
		t.Fatal("cpu,cpuacct target exists")
	}
	if V1TargetExists(root, "memory", unit) {
		t.Fatal("no memory dir in the fake")
	}
	argv := V1MigrateCommand(root, "cpu", unit, []string{"1", "2"})
	if argv[0] != "sudo" || argv[1] != "-n" || argv[len(argv)-3] != filepath.Join(root, "cpu,cpuacct", unit, "cgroup.procs") {
		t.Fatalf("argv: %q", argv)
	}
}

func TestV1MigrateScriptSkipsVanishedPids(t *testing.T) {
	// Run the script part without sudo. A write that fails (here: the target's
	// directory does not exist, like a pid that exited) must not abort the
	// rest, and the command must still succeed; verification decides.
	argv := V1MigrateCommand("/nonexistent", "memory", unit, []string{"1", "2"})
	out, err := exec.Command(argv[2], argv[3:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	target := filepath.Join(t.TempDir(), "cgroup.procs")
	put(t, target, "")
	script := argv[4]
	if out, err := exec.Command("sh", "-c", script, "sh", target, "7", "8").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, _ := os.ReadFile(target)
	// A regular file keeps only the last write; the kernel file takes each.
	if strings.TrimSpace(string(b)) != "8" {
		t.Fatalf("got %q", b)
	}
}

// fakeRun stands in for sudo: it "moves" each pid by rewriting the
// controller's line in its fake /proc/<pid>/cgroup. skip pids stay put.
func fakeRun(t *testing.T, root, proc string, skip map[string]bool, after func(call int)) (func([]string) error, *int) {
	calls := 0
	return func(argv []string) error {
		calls++
		target := argv[6]
		ctrl := "memory"
		if strings.Contains(target, "cpu") {
			ctrl = "cpu,cpuacct"
		}
		for _, pid := range argv[7:] {
			if skip[pid] {
				continue
			}
			p := filepath.Join(proc, pid, "cgroup")
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var lines []string
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if parts := strings.SplitN(l, ":", 3); len(parts) == 3 && parts[1] == ctrl {
					l = parts[0] + ":" + ctrl + ":" + unit
				}
				lines = append(lines, l)
			}
			put(t, p, strings.Join(lines, "\n")+"\n")
		}
		if after != nil {
			after(calls)
		}
		return nil
	}, &calls
}

func TestPlaceV1MovesEveryProcess(t *testing.T) {
	root, proc := fakeV1(t)
	run, _ := fakeRun(t, root, proc, nil, nil)
	moved, err := PlaceV1(root, proc, unit, "100", run)
	if err != nil || moved["cpu"] != 3 || moved["memory"] != 0 {
		t.Fatalf("moved=%v err=%v", moved, err)
	}
	if mis, _ := V1Misplaced(root, proc, unit); len(mis["cpu"]) != 0 {
		t.Fatalf("still misplaced: %v", mis)
	}
}

func TestPlaceV1FailsWhenAProcessStaysOutside(t *testing.T) {
	root, proc := fakeV1(t)
	run, _ := fakeRun(t, root, proc, map[string]bool{"201": true}, nil)
	_, err := PlaceV1(root, proc, unit, "100", run)
	if err == nil || !strings.Contains(err.Error(), "201") || !strings.Contains(err.Error(), "does NOT cover") {
		t.Fatalf("a process left outside must fail, naming it: %v", err)
	}
}

func TestPlaceV1FailsWithoutATargetCgroup(t *testing.T) {
	root, proc := fakeV1(t)
	os.RemoveAll(filepath.Join(root, "cpu,cpuacct"))
	run, calls := fakeRun(t, root, proc, nil, nil)
	if _, err := PlaceV1(root, proc, unit, "100", run); err == nil || !strings.Contains(err.Error(), "made no cpu cgroup") {
		t.Fatalf("got %v", err)
	}
	if *calls != 0 {
		t.Fatal("nothing must be written when there is nowhere to write it")
	}
}

func TestPlaceV1FailsOnAnEmptyProcessList(t *testing.T) {
	root, proc := t.TempDir(), t.TempDir()
	put(t, filepath.Join(root, "systemd", unit, "cgroup.procs"), "")
	put(t, filepath.Join(root, "cpu,cpuacct", unit, "cgroup.procs"), "")
	run, _ := fakeRun(t, root, proc, nil, nil)
	if _, err := PlaceV1(root, proc, unit, "100", run); err == nil || !strings.Contains(err.Error(), "no processes") {
		t.Fatalf("nothing to check is not success: %v", err)
	}
}

func TestPlaceV1RequiresTheUserManager(t *testing.T) {
	root, proc := fakeV1(t)
	run, _ := fakeRun(t, root, proc, nil, nil)
	if _, err := PlaceV1(root, proc, unit, "999", run); err == nil || !strings.Contains(err.Error(), "user manager (pid 999)") {
		t.Fatalf("a manager outside its own unit means an unexpected layout: %v", err)
	}
}

func TestPlaceV1CatchesAForkOnTheSecondPass(t *testing.T) {
	root, proc := fakeV1(t)
	// A process forked during the first move: in the unit, but still in
	// cpu:/user.slice, inheriting from a parent that had not moved yet.
	run, calls := fakeRun(t, root, proc, nil, func(call int) {
		if call == 1 {
			put(t, filepath.Join(root, "systemd", unit, "buzzfleetd.service", "cgroup.procs"), "300\n400\n")
			put(t, filepath.Join(proc, "400", "cgroup"), "12:memory:"+unit+"\n9:cpu,cpuacct:/user.slice\n")
		}
	})
	moved, err := PlaceV1(root, proc, unit, "100", run)
	if err != nil || moved["cpu"] != 4 || *calls != 2 {
		t.Fatalf("moved=%v calls=%d err=%v", moved, *calls, err)
	}
}

func TestPlaceV1ReportsAFailedMove(t *testing.T) {
	root, proc := fakeV1(t)
	_, err := PlaceV1(root, proc, unit, "100", func([]string) error { return os.ErrPermission })
	if err == nil || !strings.Contains(err.Error(), "moving processes into the cpu cgroup failed") {
		t.Fatalf("a refused sudo must surface as such: %v", err)
	}
}
