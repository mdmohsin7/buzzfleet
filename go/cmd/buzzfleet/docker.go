package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// ── Docker proxies ────────────────────────────────────────────────────────────

// dockerLoop keeps one proxy running per agent, picking up agents added since
// the supervisor started.
func dockerLoop(ctx context.Context, h *host.Host, cfg *fleet.Config) {
	if cfg.Limits.NoDockerProxy {
		return
	}
	l := fleet.SizeLimits(host.NumCPU(), host.MemTotalMB(), cfg.Limits)
	var mu sync.Mutex
	running := map[string]bool{}
	ensure := func() {
		for _, hn := range h.AllHarnesses() {
			n := hn.Instance()
			mu.Lock()
			if running[n] {
				mu.Unlock()
				continue
			}
			running[n] = true
			mu.Unlock()
			p := &host.DockerProxy{Agent: hn.Agent, Community: hn.Community, Socket: host.DockerSocketFor(n),
				Upstream: host.UpstreamDocker(), Limits: l, Logf: logf}
			go func() {
				if err := p.Serve(ctx); err != nil {
					logf("docker proxy %s: %v", n, err)
				}
				mu.Lock()
				delete(running, n) // retried on the next pass
				mu.Unlock()
			}()
		}
	}
	logf("docker proxies: containers capped at %.2g CPU / %dM each", l.DockerCPUs, l.DockerMemoryMB)
	ensure()
	t := time.NewTicker(time.Duration(cfg.HealthSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ensure()
		}
	}
}

// dockerFindings flags agents whose containers are not going through the proxy.
// An agent reads DOCKER_HOST once, at start, so one started before the proxy
// existed keeps creating uncapped containers until it is restarted.
func dockerFindings(h *host.Host) []finding {
	if limitsConfig(h).NoDockerProxy {
		return nil
	}
	var out []finding
	for _, hn := range h.AllHarnesses() {
		n := hn.Instance()
		pid := h.MainPID(n)
		if pid == "" {
			continue
		}
		env, _ := os.ReadFile("/proc/" + pid + "/environ")
		want := "DOCKER_HOST=unix://" + host.DockerSocketFor(n)
		if !strings.Contains("\x00"+string(env)+"\x00", "\x00"+want+"\x00") {
			out = append(out, finding{msg: fmt.Sprintf(
				"%s: started without the Docker proxy — its containers are uncapped until it restarts", n)})
		} else if _, err := os.Stat(host.DockerSocketFor(n)); err != nil {
			out = append(out, finding{msg: fmt.Sprintf(
				"%s: its Docker proxy socket is missing — is buzzfleetd running? Docker commands will fail", n)})
		}
	}
	return out
}
