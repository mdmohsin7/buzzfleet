package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// ── Docker guardrail ──────────────────────────────────────────────────────────
//
// Containers are created by dockerd, not by the agent, so they run in Docker's
// cgroups and escape every ceiling on the agent's own. An agent running a full
// test suite in a container is the one load nothing else can see.
//
// The supervisor puts a Docker API proxy in front of each agent and rewrites
// container creation on the way through: a default ceiling where none was asked
// for, a clamp where more was, and labels saying which agent made it. It is a
// guardrail, not a boundary — an agent in the docker group can still reach the
// real socket — but agents use whatever DOCKER_HOST says, and that is enough to
// stop the accident.

const (
	LabelAgent   = "buzzfleet.agent"
	LabelCreated = "buzzfleet.created"
)

var (
	createPathRe = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)
	updatePathRe = regexp.MustCompile(`^(/v[0-9.]+)?/containers/[^/]+/update$`)
)

// RewriteKind says which requests the proxy rewrites.
func RewriteKind(method, path string) string {
	if method != "POST" {
		return ""
	}
	switch {
	case createPathRe.MatchString(path):
		return "create"
	case updatePathRe.MatchString(path):
		return "update"
	}
	return ""
}

const cpuPeriod = 100000 // the kernel's default CFS period, in µs

// ClampContainer rewrites a container create or update body to fit the caps.
//
// Only ever lowers: a request for less than the cap is kept as asked. The body
// is handled as a generic map so that fields this code has never heard of pass
// through untouched — the Docker API grows, and a proxy that drops what it does
// not understand would break the next CLI release.
//
// For "create" the HostConfig is nested; for "update" the resource fields sit at
// the top level. Labels are only added on create; they cannot be changed later.
func ClampContainer(kind string, body []byte, l Limits, agent string, now int64) ([]byte, []string, error) {
	var doc map[string]any
	if len(body) == 0 {
		doc = map[string]any{}
	} else {
		// UseNumber: a float64 round-trip would corrupt large integers in
		// fields this code never looks at.
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, nil, fmt.Errorf("container %s body is not JSON: %w", kind, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}

	res := doc
	if kind == "create" {
		hc, _ := doc["HostConfig"].(map[string]any)
		if hc == nil {
			hc = map[string]any{}
			doc["HostConfig"] = hc
		}
		res = hc
	}

	var changes []string
	note := func(f string, a ...any) { changes = append(changes, fmt.Sprintf(f, a...)) }

	capNano := int64(l.DockerCPUs * 1e9)
	if capNano > 0 {
		nano := num(res["NanoCpus"])
		quota, period := num(res["CpuQuota"]), num(res["CpuPeriod"])
		switch {
		case quota > 0:
			// Docker refuses NanoCpus alongside CpuQuota, so clamp in the form
			// the caller chose.
			if period <= 0 {
				period = cpuPeriod
			}
			if limit := capNano * period / 1e9; quota > limit {
				res["CpuQuota"] = limit
				note("cpu quota %d -> %d", quota, limit)
			}
		case nano > capNano:
			res["NanoCpus"] = capNano
			note("cpus %.2f -> %.2f", float64(nano)/1e9, l.DockerCPUs)
		case nano <= 0 && kind == "create":
			res["NanoCpus"] = capNano
			note("cpus unset -> %.2f", l.DockerCPUs)
		}
	}

	capMem := int64(l.DockerMemoryMB) << 20
	if capMem > 0 {
		mem := num(res["Memory"])
		switch {
		case mem > capMem:
			res["Memory"] = capMem
			note("memory %dM -> %dM", mem>>20, l.DockerMemoryMB)
			mem = capMem
		case mem <= 0 && kind == "create":
			res["Memory"] = capMem
			note("memory unset -> %dM", l.DockerMemoryMB)
			mem = capMem
		}
		// Swap is memory+swap in Docker's terms. Unlimited swap turns a memory
		// cap into a disk-thrashing one, which on a small box is worse.
		if mem > 0 {
			if sw, ok := res["MemorySwap"]; ok {
				if v := num(sw); v == -1 || v > 2*mem || (v > 0 && v < mem) {
					res["MemorySwap"] = 2 * mem
					note("memory+swap %d -> %dM", v, (2*mem)>>20)
				}
			}
		}
	}

	if kind == "create" {
		labels, _ := doc["Labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			doc["Labels"] = labels
		}
		labels[LabelAgent] = agent
		labels[LabelCreated] = strconv.FormatInt(now, 10)
	}

	out, err := json.Marshal(doc)
	return out, changes, err
}

func num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	}
	return 0
}
