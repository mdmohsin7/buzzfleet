package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

func TestTurnLockExcludesAndRecordsHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	path := filepath.Join(dir, "turn-agent-a.lock")
	a, err := OpenTurnLock(path, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenTurnLock(path, "agent-a--b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	fi, _ := os.Stat(dir)
	if fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm() != 0o770 {
		t.Fatalf("run dir mode %v", fi.Mode())
	}
	if lf, _ := os.Stat(path); lf.Mode().Perm() != 0o660 {
		t.Fatalf("lock file mode %o: a waiter in the agent's group must be able to record itself", lf.Mode().Perm())
	}

	if ok, err := a.TryLock(); !ok || err != nil {
		t.Fatal("a takes the free lock")
	}
	if ok, _ := b.TryLock(); ok {
		t.Fatal("b must not get a lock a holds")
	}
	if ReadHolder(path) == nil {
		t.Fatal("held: someone is reported")
	}
	_ = a.RecordHolder(fleet.Holder{Harness: "agent-a", PID: 1, Since: 100})
	if h := ReadHolder(path); h == nil || h.Harness != "agent-a" || h.Since != 100 {
		t.Fatalf("holder: %+v", h)
	}
	_ = a.Unlock()
	if ReadHolder(path) != nil {
		t.Fatal("released: nobody holds it, whatever was written")
	}
	if ok, _ := b.TryLock(); !ok {
		t.Fatal("b takes it once a is done")
	}
}

func TestStaleHolderRecordIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn-agent-a.lock")
	// A record left by a holder that died without unlocking.
	_ = os.WriteFile(path, []byte(`{"harness":"agent-a","pid":1,"since":1}`), 0o660)
	if ReadHolder(path) != nil {
		t.Fatal("an unheld lock has no holder, whatever the file says")
	}
}

func TestWaitRecords(t *testing.T) {
	dir := t.TempDir()
	h := &Host{FleetHome: dir}
	lock := h.TurnLockPath("agent-a")
	mine, other := h.TurnWaitPath("agent-a"), h.TurnWaitPath("agent-a--b")
	if err := WriteWait(other, 500); err != nil {
		t.Fatal(err)
	}
	if ReadWait(other) != 500 {
		t.Fatal("a live waiter (this process) is read back")
	}
	ws := OtherWaiters(lock, mine)
	if len(ws) != 1 || ws[0].Since != 500 || !ws[0].Alive {
		t.Fatalf("others: %+v", ws)
	}
	if len(OtherWaiters(lock, other)) != 0 {
		t.Fatal("a harness is not its own competition")
	}
	// A record from a gate that died: its pid no longer exists.
	_ = os.WriteFile(other, []byte("400 2147483646\n"), 0o660)
	if ReadWait(other) != 0 || OtherWaiters(lock, mine)[0].Alive {
		t.Fatal("a dead gate's wait record must not count")
	}
	_ = WriteWait(other, 0)
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("cleared")
	}
}
