package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the gate and as a fake coding agent, so the
// gate is exercised through real pipes.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fake-agent" && os.Getenv(envGateReal) == "" {
		fakeStreamingAgent()
		return
	}
	if os.Getenv("BUZZFLEET_TEST_GATE") == "1" && os.Getenv(envGateReal) != "" {
		runGate()
		return
	}
	os.Exit(m.Run())
}

// fakeStreamingAgent writes a burst of session/update lines before it next
// reads, the way a coding agent streams mid-turn, then echoes what it reads.
func fakeStreamingAgent() {
	w := bufio.NewWriter(os.Stdout)
	pad := strings.Repeat("x", 2000)
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","n":%d,"pad":"%s"}}`+"\n", i, pad)
	}
	w.Flush()
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			fmt.Printf(`{"jsonrpc":"2.0","method":"debug","params":{"len":%d}}`+"\n", len(line))
		}
		if err != nil {
			return
		}
	}
}

func TestGateNeverDeadlocksOnABigLine(t *testing.T) {
	dir := t.TempDir()
	self, _ := os.Executable()
	cmd := exec.Command(self, "fake-agent")
	cmd.Env = append(os.Environ(),
		"BUZZFLEET_TEST_GATE=1",
		envGateReal+"="+self,
		envGateLock+"="+filepath.Join(dir, "run", "turn-a.lock"),
		envGateWait+"="+filepath.Join(dir, "run", "turn-a.wait-a"),
		envGateShimDir+"="+filepath.Join(dir, "shim"),
		"BUZZ_FLEET_HARNESS=a",
	)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	// One multi-megabyte line, far past any pipe buffer, while the agent is
	// busy writing and not reading.
	big, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/notice",
		"params": map[string]string{"blob": strings.Repeat("y", 4<<20)}})
	go func() { _, _ = in.Write(append(big, '\n')) }()

	got := make(chan int, 1)
	go func() {
		n := 0
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				n++
				if strings.Contains(string(line), `"method":"debug"`) {
					got <- n
					return
				}
			}
			if err != nil {
				got <- n
				return
			}
		}
	}()
	select {
	case n := <-got:
		if n != 2001 {
			t.Fatalf("got %d lines, want all 2000 updates and the echo of the big line", n)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("deadlocked: the gate stopped moving lines in either direction")
	}
}
