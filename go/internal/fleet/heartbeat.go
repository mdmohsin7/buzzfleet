package fleet

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ── out-of-band heartbeat ─────────────────────────────────────────────────────
//
// Everything else the supervisor says goes through the relay, so when the relay
// or the host itself is wedged, nothing says anything — and silence looks the
// same as "all fine". A heartbeat pushed straight to a monitoring endpoint turns
// that silence into the alert: the receiver fires when values go bad, and when
// they stop arriving at all.
//
// The push is a plain HTTPS POST, in one of two bodies:
//   - "json": one self-describing document, for any webhook or receiver.
//   - "influx": InfluxDB line protocol, which Grafana Cloud (Mimir) ingests at
//     /api/v1/push/influx/write as Prometheus series named
//     <measurement>_<field>. Remote-write and OTLP would need protobuf and
//     snappy, or an SDK, in a static binary — for one sample a minute.

// HeartbeatConfig is where the heartbeat goes. Secrets are read from files so
// they never sit in fleetd.json or a process argument.
type HeartbeatConfig struct {
	URL             string `json:"url"`
	Format          string `json:"format,omitempty"` // "json" (default) or "influx"
	IntervalSecs    int    `json:"intervalSecs,omitempty"`
	BasicUser       string `json:"basicUser,omitempty"` // e.g. a Grafana Cloud instance ID
	PasswordFile    string `json:"passwordFile,omitempty"`
	BearerTokenFile string `json:"bearerTokenFile,omitempty"`
}

func (c HeartbeatConfig) Enabled() bool { return c.URL != "" }

func (c HeartbeatConfig) Interval() int {
	if c.IntervalSecs <= 0 {
		return 60
	}
	return c.IntervalSecs
}

// Validate catches the mistakes that would otherwise only show up as an alert
// that never fires.
func (c HeartbeatConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
		return fmt.Errorf("heartbeat url must be http(s): %q", c.URL)
	}
	p, err := url.Parse(c.URL)
	if err != nil || p.Host == "" {
		// Said once here, rather than as a parse error quoting the whole
		// URL in the log on every failed beat.
		return fmt.Errorf("heartbeat url does not parse")
	}
	switch c.Format {
	case "", "json", "influx":
	default:
		return fmt.Errorf("heartbeat format must be json or influx, not %q", c.Format)
	}
	if c.BasicUser != "" && c.PasswordFile == "" {
		return fmt.Errorf("heartbeat basicUser is set but passwordFile is not")
	}
	if c.BasicUser != "" && c.BearerTokenFile != "" {
		return fmt.Errorf("heartbeat: use basic auth or a bearer token, not both")
	}
	creds := c.BasicUser != "" || c.BearerTokenFile != "" || p.User != nil
	if creds && p.Scheme == "http" && !loopback(c.URL) {
		return fmt.Errorf("heartbeat: credentials over plain http would travel in cleartext; use https")
	}
	return nil
}

// HostMetrics is one reading of the machine.
type HostMetrics struct {
	Load1, Load5, Load15 float64
	CPUs                 int
	MemTotal, MemAvail   uint64 // bytes
	SwapTotal, SwapFree  uint64
	DiskTotal, DiskFree  uint64 // the filesystem holding the fleet home
	UptimeSecs           int64
}

// AgentReading is one agent's state as of the last health pass.
type AgentReading struct {
	Name     string
	State    State
	Detail   string
	UpSecs   int64 // 0 when not running
	Restarts int   // heals in the current window
}

// Snapshot is what one heartbeat carries.
type Snapshot struct {
	Host    string
	At      int64 // unix seconds
	Seq     uint64
	Version string
	Metrics HostMetrics
	// Age of the agent readings: the health pass talks to the relay and can
	// stall with it. A heartbeat that keeps arriving with an ever older
	// reading says "host fine, relay not" — a different alert from silence.
	AssessedAgo int64
	RelayOK     bool
	Agents      []AgentReading
}

// Healthy is 1 for the states that need nobody's attention.
func healthy(s State) int {
	if s == StateOK {
		return 1
	}
	return 0
}

// EncodeJSON is the "json" body.
func EncodeJSON(s Snapshot) ([]byte, error) {
	type agent struct {
		Name     string `json:"name"`
		State    State  `json:"state"`
		Healthy  bool   `json:"healthy"`
		Detail   string `json:"detail,omitempty"`
		UpSecs   int64  `json:"up_seconds"`
		Restarts int    `json:"restarts_in_window"`
	}
	doc := struct {
		Kind        string  `json:"kind"`
		Host        string  `json:"host"`
		At          int64   `json:"at"`
		Seq         uint64  `json:"seq"`
		Version     string  `json:"version"`
		RelayOK     bool    `json:"relay_ok"`
		AssessedAgo int64   `json:"assessed_seconds_ago"`
		Load1       float64 `json:"load1"`
		Load5       float64 `json:"load5"`
		Load15      float64 `json:"load15"`
		CPUs        int     `json:"cpus"`
		MemTotal    uint64  `json:"mem_total_bytes"`
		MemAvail    uint64  `json:"mem_available_bytes"`
		SwapTotal   uint64  `json:"swap_total_bytes"`
		SwapFree    uint64  `json:"swap_free_bytes"`
		DiskTotal   uint64  `json:"disk_total_bytes"`
		DiskFree    uint64  `json:"disk_free_bytes"`
		Uptime      int64   `json:"uptime_seconds"`
		AgentsOK    int     `json:"agents_ok"`
		AgentsTotal int     `json:"agents_total"`
		Agents      []agent `json:"agents"`
	}{
		Kind: "buzzfleet.heartbeat", Host: s.Host, At: s.At, Seq: s.Seq, Version: s.Version,
		RelayOK: s.RelayOK, AssessedAgo: s.AssessedAgo,
		Load1: s.Metrics.Load1, Load5: s.Metrics.Load5, Load15: s.Metrics.Load15, CPUs: s.Metrics.CPUs,
		MemTotal: s.Metrics.MemTotal, MemAvail: s.Metrics.MemAvail,
		SwapTotal: s.Metrics.SwapTotal, SwapFree: s.Metrics.SwapFree,
		DiskTotal: s.Metrics.DiskTotal, DiskFree: s.Metrics.DiskFree, Uptime: s.Metrics.UptimeSecs,
		AgentsTotal: len(s.Agents), Agents: []agent{},
	}
	for _, a := range sortedAgents(s.Agents) {
		doc.AgentsOK += healthy(a.State)
		doc.Agents = append(doc.Agents, agent{a.Name, a.State, a.State == StateOK, a.Detail, a.UpSecs, a.Restarts})
	}
	return json.Marshal(doc)
}

// EncodeInflux is the "influx" body: one line per series, timestamps in ns.
//
// Mimir keeps numeric fields only, so every state is also a number: the agent's
// state is a tag and its health a 0/1 field, which makes both "which agents are
// limited" and "how many agents are unhealthy" one query each. String fields
// are left out rather than silently dropped by the receiver.
func EncodeInflux(s Snapshot) []byte {
	ts := strconv.FormatInt(s.At, 10) + "000000000"
	// Tags in key order, as line protocol recommends.
	host := "host=" + escTag(s.Host)
	var b strings.Builder
	line := func(measurement, tags string, fields ...string) {
		b.WriteString(measurement)
		b.WriteString(",")
		b.WriteString(tags)
		b.WriteString(" ")
		b.WriteString(strings.Join(fields, ","))
		b.WriteString(" ")
		b.WriteString(ts)
		b.WriteString("\n")
	}
	relay := 0
	if s.RelayOK {
		relay = 1
	}
	line("buzzfleet_heartbeat", host,
		"up=1", "seq="+u(s.Seq), "relay_ok="+strconv.Itoa(relay), "assessed_seconds_ago="+strconv.FormatInt(s.AssessedAgo, 10))
	m := s.Metrics
	line("buzzfleet_host", host,
		"load1="+f(m.Load1), "load5="+f(m.Load5), "load15="+f(m.Load15), "cpus="+strconv.Itoa(m.CPUs),
		"mem_total_bytes="+u(m.MemTotal), "mem_available_bytes="+u(m.MemAvail),
		"swap_total_bytes="+u(m.SwapTotal), "swap_free_bytes="+u(m.SwapFree),
		"disk_total_bytes="+u(m.DiskTotal), "disk_free_bytes="+u(m.DiskFree),
		"uptime_seconds="+strconv.FormatInt(m.UptimeSecs, 10))
	ok := 0
	for _, a := range sortedAgents(s.Agents) {
		ok += healthy(a.State)
		agent := "agent=" + escTag(a.Name) + "," + host
		line("buzzfleet_agent", agent,
			"healthy="+strconv.Itoa(healthy(a.State)), "up_seconds="+strconv.FormatInt(a.UpSecs, 10),
			"restarts_in_window="+strconv.Itoa(a.Restarts))
		// State as an enum: every known state on every beat, 1 for the current
		// one. A pushed series has no staleness marker, so a state carried as a
		// label would leave the old series answering queries for the lookback
		// window after each change.
		for _, st := range knownStates(a.State) {
			v := "0"
			if st == a.State {
				v = "1"
			}
			line("buzzfleet_agent_state", agent+",state="+escTag(string(st)), "active="+v)
		}
	}
	line("buzzfleet_agents", host, "total="+strconv.Itoa(len(s.Agents)), "ok="+strconv.Itoa(ok))
	return []byte(b.String())
}

// AllStates is every state an agent can report, so the state enum always
// carries a full set of series.
var AllStates = []State{StateOK, StateWedged, StateStalled, StateDown, StateUnknown, StateAuth, StateLimited}

func knownStates(cur State) []State {
	for _, s := range AllStates {
		if s == cur {
			return AllStates
		}
	}
	return append(append([]State(nil), AllStates...), cur)
}

// Plain floats, never exponent form, and integers as floats: Mimir treats every
// field as a float sample anyway, and an "i" suffix only buys a type mismatch
// if a field is ever written both ways.
func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func u(v uint64) string  { return strconv.FormatUint(v, 10) }
func escTag(v string) string {
	if v == "" {
		return "none"
	}
	return strings.NewReplacer(`\`, `\\`, ",", `\,`, "=", `\=`, " ", `\ `, "\n", `\ `).Replace(v)
}

func sortedAgents(in []AgentReading) []AgentReading {
	out := append([]AgentReading(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func loopback(u string) bool {
	rest := strings.TrimPrefix(u, "http://")
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	h := rest
	if strings.HasPrefix(h, "[") {
		h = strings.TrimPrefix(strings.SplitN(h, "]", 2)[0], "[")
	} else if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
