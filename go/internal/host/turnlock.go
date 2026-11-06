package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

// The turn lock lives under the fleet home, not $XDG_RUNTIME_DIR: the runtime
// dir is private to one Linux user, and a harness running as another (a
// restricted community, later) must be able to take the same lock or its
// turns would silently not be serialised. The directory is setgid and group
// writable, and the lock file is made group read-write explicitly rather than
// trusting the umask: flock itself only needs a readable descriptor, but
// recording yourself as the holder is a write.

// RunDir is where turn locks live.
func (h *Host) RunDir() string { return filepath.Join(h.FleetHome, "run") }

// TurnLockPath is an agent's turn lock.
func (h *Host) TurnLockPath(agent string) string {
	return filepath.Join(h.RunDir(), "turn-"+agent+".lock")
}

// TurnWaitPath is where a harness records that it is waiting for the lock.
func (h *Host) TurnWaitPath(instance string) string {
	agent := fleet.ParseInstance(instance).Agent
	return filepath.Join(h.RunDir(), "turn-"+agent+".wait-"+instance)
}

// EnsureRunDir creates the run directory setgid and group-writable.
func EnsureRunDir(dir string) error {
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if want := os.ModeSetgid | 0o770; fi.Mode()&(os.ModeSetgid|os.ModePerm) != want {
		// Only the owner can chmod; anyone else gets the directory as is.
		_ = os.Chmod(dir, want)
	}
	return nil
}

// TurnLock is one harness's handle on its agent's turn lock.
type TurnLock struct {
	f        *os.File
	instance string
}

// OpenTurnLock opens (creating if need be) the agent's lock file.
func OpenTurnLock(path, instance string) (*TurnLock, error) {
	if err := EnsureRunDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o660)
	if err != nil {
		return nil, fmt.Errorf("open turn lock: %w", err)
	}
	// Explicitly, not by umask. Fails harmlessly when another user owns it.
	_ = f.Chmod(0o660)
	return &TurnLock{f: f, instance: instance}, nil
}

// TryLock takes the lock without blocking.
func (l *TurnLock) TryLock() (bool, error) {
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if err == syscall.EWOULDBLOCK {
		return false, nil
	}
	return false, err
}

// Unlock releases the lock, clearing the holder record first so nobody reads
// a stale one as current.
func (l *TurnLock) Unlock() error {
	_ = l.f.Truncate(0)
	return syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
}

// RecordHolder writes who holds the turn. Only called while holding it.
func (l *TurnLock) RecordHolder(h fleet.Holder) error {
	b, _ := json.Marshal(h)
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt(append(b, '\n'), 0)
	return err
}

// Close releases everything; the kernel would anyway when the process exits.
func (l *TurnLock) Close() error { return l.f.Close() }

// ReadHolder says who holds an agent's turn right now, or nil. A record left
// behind by a holder that died is ignored: if the lock can be taken, nobody
// holds it, whatever the file says.
func ReadHolder(path string) *fleet.Holder {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return nil
	}
	var h fleet.Holder
	b := make([]byte, 4096)
	n, _ := f.ReadAt(b, 0)
	if json.Unmarshal([]byte(strings.TrimSpace(string(b[:n]))), &h) != nil || h.Harness == "" {
		return &fleet.Holder{Harness: "?"}
	}
	return &h
}

// A harness's wait record is a file its gate holds an exclusive flock on for
// its whole life, holding the time its oldest prompt started waiting (0 when
// none is). A reader that can take a shared lock on it knows the gate is
// dead, and ignores what the file says: liveness comes from the kernel, not
// from a pid that may have been reused.

// WaitRecord is a gate's own wait record.
type WaitRecord struct{ f *os.File }

// OpenWait claims a harness's wait record.
func OpenWait(path string) (*WaitRecord, error) {
	if err := EnsureRunDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o660)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o660)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another gate is already running for this harness (%s)", path)
	}
	w := &WaitRecord{f: f}
	return w, w.Set(0)
}

// Set records how long the harness has been waiting; 0 means it is not.
func (w *WaitRecord) Set(since int64) error {
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	_, err := w.f.WriteAt([]byte(fmt.Sprintf("%d\n", since)), 0)
	return err
}

func (w *WaitRecord) Close() error { return w.f.Close() }

func readWaiter(path string) (fleet.Waiter, bool) {
	f, err := os.Open(path)
	if err != nil {
		return fleet.Waiter{}, false
	}
	defer f.Close()
	var w fleet.Waiter
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return w, false // its gate is gone
	}
	w.Alive = true
	b := make([]byte, 64)
	n, _ := f.ReadAt(b, 0)
	_, _ = fmt.Sscan(strings.TrimSpace(string(b[:n])), &w.Since)
	return w, w.Since > 0
}

// ReadWait returns since when a harness has been waiting, or 0.
func ReadWait(path string) int64 {
	if w, ok := readWaiter(path); ok {
		return w.Since
	}
	return 0
}

// OtherWaiters lists the agent's other harnesses that are waiting for its
// turn, for first-come-first-served ordering.
func OtherWaiters(lockPath, selfWaitPath string) []fleet.Waiter {
	matches, _ := filepath.Glob(strings.TrimSuffix(lockPath, ".lock") + ".wait-*")
	var out []fleet.Waiter
	for _, m := range matches {
		if m == selfWaitPath {
			continue
		}
		if w, ok := readWaiter(m); ok {
			out = append(out, w)
		}
	}
	return out
}
