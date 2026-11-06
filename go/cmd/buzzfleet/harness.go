package main

import (
	"fmt"
	"strings"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// target is one harness a command acts on.
type target struct {
	fleet.Harness
	Label  string // what to call it in output
	Config *host.AgentConfig
	host.Membership
}

// Instance is the harness's unit instance, which is what every per-unit
// helper takes.
func (t target) Instance() string { return t.Harness.Instance() }

// resolve turns "agent" into every harness of that agent, and
// "agent@community" into exactly one.
func resolve(h *host.Host, arg string) ([]target, error) {
	agent, community, all := fleet.ParseTarget(arg)
	c, err := h.AgentConfig(agent)
	if err != nil {
		return nil, err
	}
	hs := c.Harnesses()
	multi := len(hs) > 1
	var out []target
	for _, hn := range hs {
		if !all && hn.Community != community {
			continue
		}
		m, _ := c.In(hn.Community)
		out = append(out, target{Harness: hn, Label: hn.Label(multi), Config: c, Membership: m})
	}
	if len(out) == 0 {
		var names []string
		for _, hn := range hs {
			names = append(names, hn.Community)
		}
		return nil, fmt.Errorf("%s is not in a community called %q (it is in: %s)", agent, community, strings.Join(names, ", "))
	}
	return out, nil
}

func mustResolve(h *host.Host, arg string) []target {
	ts, err := resolve(h, arg)
	if err != nil {
		die("%v", err)
	}
	return ts
}

// mustOne is for commands that act in exactly one community. An agent with
// only its main community needs no "@main".
func mustOne(h *host.Host, arg, verb string) target {
	ts := mustResolve(h, arg)
	if len(ts) > 1 {
		var labels []string
		for _, t := range ts {
			labels = append(labels, t.Label)
		}
		die("%s is in several communities; say which for %s: %s", ts[0].Agent, verb, strings.Join(labels, ", "))
	}
	return ts[0]
}

// allTargets is every harness on the host, for status and health.
func allTargets(h *host.Host) []target {
	var out []target
	for _, n := range h.Agents() {
		if ts, err := resolve(h, n); err == nil {
			out = append(out, ts...)
		} else {
			// An unreadable agent.json still has a unit; show it rather
			// than hide it.
			out = append(out, target{Harness: fleet.Harness{Agent: n, Community: fleet.MainCommunity}, Label: n})
		}
	}
	return out
}
