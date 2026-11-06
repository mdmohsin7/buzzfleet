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

// WriteWait records that a harness has prompts waiting since a time, and the
// pid of its gate, or clears the record when since is zero.
func WriteWait(path string, since int64) error {
	if since == 0 {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := EnsureRunDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d %d\n", since, os.Getpid())), 0o660); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadWait returns since when a harness has been waiting, or 0. A wait file
// left by a gate that died is ignored unless the harness is still running:
// the caller checks that.
func ReadWait(path string) int64 {
	w, ok := readWaiter(path)
	if !ok || !w.Alive {
		return 0
	}
	return w.Since
}

func readWaiter(path string) (fleet.Waiter, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return fleet.Waiter{}, false
	}
	var w fleet.Waiter
	_, _ = fmt.Sscan(strings.TrimSpace(string(b)), &w.Since, &w.PID)
	// A gate that died leaves its record; only a running one is waiting.
	w.Alive = w.PID > 0 && syscall.Kill(w.PID, 0) != syscall.ESRCH
	return w, w.Since > 0
}

// OtherWaiters lists the agent's other harnesses that are waiting for its
// turn, for first-come-first-served ordering.
func OtherWaiters(lockPath, selfWaitPath string) []fleet.Waiter {
	matches, _ := filepath.Glob(strings.TrimSuffix(lockPath, ".lock") + ".wait-*")
	var out []fleet.Waiter
	for _, m := range matches {
		if m == selfWaitPath || strings.HasSuffix(m, ".tmp") {
			continue
		}
		if w, ok := readWaiter(m); ok {
			out = append(out, w)
		}
	}
	return out
}
