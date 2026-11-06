package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	LabelAgent     = "buzzfleet.agent"
	LabelCommunity = "buzzfleet.community"
	LabelCreated   = "buzzfleet.created"
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

// ErrAmbiguous is returned for a body that names one field in two spellings.
var ErrAmbiguous = fmt.Errorf("container body names a field more than once (in different letter case)")

// canonical renames a field to its canonical spelling and reports its value.
//
// dockerd decodes with encoding/json, which matches keys case-insensitively, so
// "memory" and "hostConfig" mean exactly what "Memory" and "HostConfig" do. A
// proxy that only looked at the canonical spelling would write its cap next to
// the caller's lowercase key, and whichever came last in the JSON would win.
// Two spellings of one field are refused outright: which of them dockerd honours
// depends on details nobody should have to reason about to trust a cap.
func canonical(m map[string]any, key string) (any, error) {
	var found []string
	for k := range m {
		if strings.EqualFold(k, key) {
			found = append(found, k)
		}
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		v := m[found[0]]
		if found[0] != key {
			delete(m, found[0])
			m[key] = v
		}
		return v, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(found, ", "))
}

// ClampContainer rewrites a container create or update body to fit the caps.
//
// Only ever lowers: a request for less than the cap is kept as asked. The body
// is handled as a generic map so that fields this code has never heard of pass
// through untouched — the Docker API grows, and a proxy that drops what it does
// not understand would break the next CLI release.
//
// For "create" the HostConfig is nested; for "update" the resource fields sit at
// the top level. Labels are only added on create; they cannot be changed later.
// Any error means the request must be refused, not passed on.
func ClampContainer(kind string, body []byte, l Limits, agent string, now int64) ([]byte, []string, error) {
	return ClampContainerFor(kind, body, l, agent, "", now)
}

// ClampContainerFor is ClampContainer for a harness in a named community,
// which is labelled too so cleanup can tell an agent's communities apart.
func ClampContainerFor(kind string, body []byte, l Limits, agent, community string, now int64) ([]byte, []string, error) {
	var doc map[string]any
	if len(bytes.TrimSpace(body)) == 0 {
		doc = map[string]any{}
	} else {
		// UseNumber: a float64 round-trip would corrupt large integers in
		// fields this code never looks at.
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, nil, fmt.Errorf("container %s body is not a JSON object: %w", kind, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}

	res := doc
	if kind == "create" {
		v, err := canonical(doc, "HostConfig")
		if err != nil {
			return nil, nil, err
		}
		hc, _ := v.(map[string]any)
		if hc == nil {
			hc = map[string]any{}
			doc["HostConfig"] = hc
		}
		res = hc
	}
	fields := map[string]any{}
	for _, k := range []string{"NanoCpus", "CpuQuota", "CpuPeriod", "Memory", "MemorySwap"} {
		v, err := canonical(res, k)
		if err != nil {
			return nil, nil, err
		}
		fields[k] = v
	}

	// A negative value means "unlimited" to dockerd, on update as well as
	// create, and it is honoured even on a container capped in another form:
	// CpuQuota -1 on a NanoCpus container writes "max" to the cgroup. Nothing
	// legitimate needs an agent to lift its own cap, so these are refused.
	for _, k := range []string{"NanoCpus", "CpuQuota", "CpuPeriod", "Memory"} {
		if fields[k] != nil && num(fields[k]) < 0 {
			return nil, nil, fmt.Errorf("%s %d would lift the cap; buzzfleet does not allow unlimited containers", k, num(fields[k]))
		}
	}

	var changes []string
	note := func(f string, a ...any) { changes = append(changes, fmt.Sprintf(f, a...)) }

	capNano := int64(l.DockerCPUs * 1e9)
	if capNano > 0 {
		nano := num(fields["NanoCpus"])
		quota, period := num(fields["CpuQuota"]), num(fields["CpuPeriod"])
		switch {
		case quota > 0 || period > 0:
			// Docker refuses NanoCpus alongside CpuQuota or CpuPeriod, so clamp
			// in the form the caller chose.
			if period <= 0 {
				period = cpuPeriod
			}
			if limit := capNano * period / 1e9; quota <= 0 || quota > limit {
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
		mem := num(fields["Memory"])
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
		// cap into a disk-thrashing one, which on a small box is worse. An
		// update may set swap alone; bound it by the cap then.
		known := mem > 0
		if !known {
			mem = capMem
		}
		if fields["MemorySwap"] != nil {
			// Raising swap to match memory is only right when the memory is
			// known; otherwise a small container's swap would be raised.
			if v := num(fields["MemorySwap"]); v < 0 || v > 2*mem || (known && v > 0 && v < mem) {
				res["MemorySwap"] = 2 * mem
				note("memory+swap %d -> %dM", v, (2*mem)>>20)
			}
		}
	}

	if kind == "create" {
		v, err := canonical(doc, "Labels")
		if err != nil {
			return nil, nil, err
		}
		labels, _ := v.(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			doc["Labels"] = labels
		}
		// Label keys are case-sensitive in Docker, so these exact keys are the
		// ones cleanup reads, and the caller's own values for them are replaced.
		labels[LabelAgent] = agent
		labels[LabelCreated] = strconv.FormatInt(now, 10)
		if community != "" {
			labels[LabelCommunity] = community
		}
	}

	out, err := json.Marshal(doc)
	return out, changes, err
}

func num(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		// "4e9" is a valid JSON number that Int64 will not parse.
		f, _ := n.Float64()
		return int64(f)
	case int64:
		return n
	}
	return 0
}
