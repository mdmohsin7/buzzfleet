package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

func snap() Snapshot {
	return Snapshot{
		Host: "host-1", At: 1767225600, Seq: 7, Version: "v0.0.0-test",
		Metrics: HostMetrics{Load1: 0.5, Load5: 1.25, Load15: 2, CPUs: 2,
			MemTotal: 12 << 30, MemAvail: 8 << 30, SwapTotal: 8 << 30, SwapFree: 7 << 30,
			DiskTotal: 200 << 30, DiskFree: 40 << 30, UptimeSecs: 86400},
		AssessedAgo: 12, RelayOK: true,
		Agents: []AgentReading{
			{Name: "agent-b", State: StateWedged, Detail: "running but absent from relay", UpSecs: 3600},
			{Name: "agent-a", State: StateOK, UpSecs: 7200, Restarts: 1},
		},
	}
}

func TestInfluxBody(t *testing.T) {
	got := string(EncodeInflux(snap()))
	want := strings.Join([]string{
		"buzzfleet_heartbeat,host=host-1 up=1,seq=7,relay_ok=1,assessed_seconds_ago=12 1767225600000000000",
		"buzzfleet_host,host=host-1 load1=0.5,load5=1.25,load15=2,cpus=2,mem_total_bytes=12884901888,mem_available_bytes=8589934592,swap_total_bytes=8589934592,swap_free_bytes=7516192768,disk_total_bytes=214748364800,disk_free_bytes=42949672960,uptime_seconds=86400 1767225600000000000",
		"buzzfleet_agent,host=host-1,agent=agent-a,state=ok healthy=1,up_seconds=7200,restarts_in_window=1 1767225600000000000",
		"buzzfleet_agent,host=host-1,agent=agent-b,state=wedged healthy=0,up_seconds=3600,restarts_in_window=0 1767225600000000000",
		"buzzfleet_agents,host=host-1 total=2,ok=1 1767225600000000000",
	}, "\n") + "\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestInfluxHasNoStringFieldsAndEscapesTags(t *testing.T) {
	s := snap()
	s.Host = "a host,with=odd chars"
	s.Agents = []AgentReading{{Name: "", State: StateDown}}
	body := string(EncodeInflux(s))
	if strings.Contains(body, `"`) {
		t.Fatalf("Mimir drops string fields; there must be none:\n%s", body)
	}
	if !strings.Contains(body, `host=a\ host\,with\=odd\ chars`) || !strings.Contains(body, "agent=none,state=down") {
		t.Fatalf("tags not escaped:\n%s", body)
	}
	if strings.Count(body, "\n") != 4 {
		t.Fatalf("one line per series:\n%s", body)
	}
}

func TestInfluxNeverUsesExponents(t *testing.T) {
	s := snap()
	s.Metrics.Load1 = 1e-7
	if body := string(EncodeInflux(s)); strings.Contains(body, "e-") || strings.Contains(body, "e+") {
		t.Fatalf("exponent in body:\n%s", body)
	}
}

func TestJSONBody(t *testing.T) {
	b, err := EncodeJSON(snap())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["kind"] != "buzzfleet.heartbeat" || doc["host"] != "host-1" || doc["agents_ok"].(float64) != 1 || doc["relay_ok"] != true {
		t.Fatalf("got %s", b)
	}
	agents := doc["agents"].([]any)
	first := agents[0].(map[string]any)
	if first["name"] != "agent-a" || first["healthy"] != true || agents[1].(map[string]any)["state"] != "wedged" {
		t.Fatalf("agents sorted by name with state: %s", b)
	}
	empty, _ := EncodeJSON(Snapshot{Host: "h"})
	if !strings.Contains(string(empty), `"agents":[]`) {
		t.Fatalf("no agents must be [], not null: %s", empty)
	}
}

func TestHeartbeatConfigValidate(t *testing.T) {
	ok := []HeartbeatConfig{
		{},
		{URL: "https://example.invalid/hb"},
		{URL: "https://example.invalid/api/v1/push/influx/write", Format: "influx", BasicUser: "123", PasswordFile: "/x"},
		{URL: "http://localhost:9/x", BearerTokenFile: "/x"},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	bad := []HeartbeatConfig{
		{URL: "example.invalid/hb"},
		{URL: "https://x", Format: "otlp"},
		{URL: "https://x", BasicUser: "123"},
		{URL: "https://x", BasicUser: "123", PasswordFile: "/a", BearerTokenFile: "/b"},
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Fatalf("%+v should be rejected", c)
		}
	}
	if (HeartbeatConfig{}).Interval() != 60 || (HeartbeatConfig{IntervalSecs: 30}).Interval() != 30 {
		t.Fatal("interval default")
	}
}
