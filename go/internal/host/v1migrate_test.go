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
