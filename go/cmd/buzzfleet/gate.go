package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// The turn gate runs between buzz-acp and the coding agent, under the coding
// agent's own name.
//
// buzz-acp decides things from the agent command's name: which arguments it
// takes, whether Codex gets its network configuration, what turn metrics call
// the harness. Wrapping the command as `buzzfleet acp-gate -- claude-agent-acp`
// would change that name and quietly change those decisions. So run-agent
// puts a link named exactly like the real command first in line, pointing at
// this binary; started under that name, buzzfleet is the gate, and it starts
// the real command behind itself.

const (
	envGateReal    = "BUZZ_FLEET_GATE_REAL"
	envGateLock    = "BUZZ_FLEET_GATE_LOCK"
	envGateWait    = "BUZZ_FLEET_GATE_WAIT"
	envGateShimDir = "BUZZ_FLEET_GATE_SHIMDIR"
	envNoGate      = "BUZZ_FLEET_NO_GATE"
)

// invokedAsGate reports whether this process is the gate: started through a
// link named like the real agent command that run-agent set up.
func invokedAsGate() bool {
	real := os.Getenv(envGateReal)
	return real != "" && filepath.Base(os.Args[0]) == filepath.Base(real)
}

// installGate points the harness's agent command at a link to this binary,
// named like the real command, and says where the real one is. It returns the
// command buzz-acp should run.
func installGate(h *host.Host, instance, agentCmd string, env []string) (string, []string, error) {
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	real, err := lookPathIn(agentCmd, path)
	if err != nil {
		return "", nil, fmt.Errorf("agent command %q not found: %w", agentCmd, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	// The run directory first, setgid and group-writable, before anything
	// below it is created with a default mode.
	if err := host.EnsureRunDir(h.RunDir()); err != nil {
		return "", nil, err
	}
	dir := filepath.Join(h.RunDir(), "shim", instance)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	link := filepath.Join(dir, filepath.Base(real))
	if cur, err := os.Readlink(link); err != nil || cur != exe {
		_ = os.Remove(link)
		if err := os.Symlink(exe, link); err != nil {
			return "", nil, err
		}
	}
	env = setEnv(env, envGateReal, real)
	env = setEnv(env, envGateLock, h.TurnLockPath(fleet.ParseInstance(instance).Agent))
	env = setEnv(env, envGateWait, h.TurnWaitPath(instance))
	env = setEnv(env, envGateShimDir, dir)
	return link, env, nil
}

func lookPathIn(cmd, path string) (string, error) {
	if strings.Contains(cmd, "/") {
		return exec.LookPath(cmd)
	}
	for _, d := range filepath.SplitList(path) {
		p := filepath.Join(d, cmd)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// childEnv is the gate's environment minus its own settings and its link
// directory, so nothing the coding agent starts can land back in the gate.
func childEnv() []string {
	shim := os.Getenv(envGateShimDir)
	var out []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case envGateReal, envGateLock, envGateWait, envGateShimDir:
			continue
		case "PATH":
			var keep []string
			for _, d := range filepath.SplitList(v) {
				if d != shim {
					keep = append(keep, d)
				}
			}
			kv = "PATH=" + strings.Join(keep, ":")
		}
		out = append(out, kv)
	}
	return out
}

func maxTurnWait() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("BUZZ_ACP_MAX_TURN_DURATION")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 2 * time.Hour // buzz-acp's default turn limit
}

func gateLog(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "buzzfleet gate: "+format+"\n", a...)
}

// runGate is the gate's whole life: start the real agent, then shuttle lines
// both ways until it exits, and exit with its status.
func runGate() {
	instance := os.Getenv("BUZZ_FLEET_HARNESS")
	lock, err := host.OpenTurnLock(os.Getenv(envGateLock), instance)
	if err != nil {
		// Fail closed: without the lock, turns could overlap.
		gateLog("%v", err)
		os.Exit(1)
	}
	waitPath := os.Getenv(envGateWait)
	_ = host.WriteWait(waitPath, 0) // whatever a previous gate left is stale

	child := exec.Command(os.Getenv(envGateReal), os.Args[1:]...)
	child.Env = childEnv()
	child.Stderr = os.Stderr
	toChild, err := child.StdinPipe()
	if err != nil {
		gateLog("%v", err)
		os.Exit(1)
	}
	fromChild, err := child.StdoutPipe()
	if err != nil {
		gateLog("%v", err)
		os.Exit(1)
	}
	if err := child.Start(); err != nil {
		gateLog("start %s: %v", os.Getenv(envGateReal), err)
		os.Exit(1)
	}

	var outMu sync.Mutex
	write := func(w io.Writer, line []byte) {
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = w.Write(append(line, '\n'))
	}
	g := &fleet.Gate{
		Lock:     lock,
		ToAgent:  func(b []byte) { write(toChild, b) },
		ToClient: func(b []byte) { write(os.Stdout, b) },
		MaxWait:  maxTurnWait(),
		Queued:   func(p fleet.Pending) { go postQueued(p) },
		Yield: func(since time.Time) bool {
			return fleet.OlderWaiter(since, host.OtherWaiters(os.Getenv(envGateLock), waitPath))
		},
		Changed: func(holding bool, waiting []fleet.Pending) {
			if holding {
				_ = lock.RecordHolder(fleet.Holder{Harness: instance, PID: os.Getpid(), Since: time.Now().Unix()})
			}
			since := int64(0)
			if !holding && len(waiting) > 0 {
				since = waiting[0].Since.Unix()
			}
			_ = host.WriteWait(waitPath, since)
		},
	}

	clientLines := readLines(os.Stdin)
	agentLines := readLines(fromChild)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for clientLines != nil || agentLines != nil {
		select {
		case line, ok := <-clientLines:
			if !ok {
				clientLines = nil
				_ = toChild.Close() // buzz-acp went away: let the agent see EOF
				continue
			}
			g.FromClient(line, time.Now())
		case line, ok := <-agentLines:
			if !ok {
				agentLines = nil
				clientLines = nil // the agent is gone; nothing left to gate
				continue
			}
			g.FromAgent(line)
		case now := <-tick.C:
			g.Tick(now)
		}
	}
	_ = host.WriteWait(waitPath, 0)
	err = child.Wait()
	_ = lock.Close()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func readLines(r io.Reader) chan []byte {
	ch := make(chan []byte, 16)
	go func() {
		defer close(ch)
		br := bufio.NewReaderSize(r, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				ch <- []byte(strings.TrimRight(string(line), "\r\n"))
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// postQueued tells the people waiting that the agent is alive and their
// message is queued, next to the message that is waiting. It uses the agent's
// own identity and relay, which the harness's environment already carries.
func postQueued(p fleet.Pending) {
	if p.Channel == "" {
		gateLog("prompt %s queued for over a minute; no channel to say so in", p.ID)
		return
	}
	args := []string{"messages", "send", "--channel", p.Channel, "--content", fleet.QueuedNotice}
	if p.ReplyTo != "" {
		args = append(args, "--reply-to", p.ReplyTo)
	}
	cmd := exec.Command("buzz", args...)
	cmd.Env = childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		gateLog("could not post the queued notice: %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
	gateLog("prompt %s queued for over a minute; said so in the channel", p.ID)
}
