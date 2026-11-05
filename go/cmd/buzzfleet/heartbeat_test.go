package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// mockMimir stands in for Grafana Cloud's influx endpoint: basic auth, line
// protocol in, 204 out. It converts lines to series names the way Mimir does,
// <measurement>_<field>, so the test asserts on what alert rules will query.
func mockMimir(t *testing.T, user, pass string, series map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/push/influx/write" || r.Method != http.MethodPost {
			http.Error(w, "not found", 404)
			return
		}
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			http.Error(w, "unauthorized", 401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			parts := strings.Split(line, " ")
			if len(parts) != 3 {
				http.Error(w, "bad line: "+line, 400)
				return
			}
			measurement, tags, _ := strings.Cut(parts[0], ",")
			for _, f := range strings.Split(parts[1], ",") {
				k, v, _ := strings.Cut(f, "=")
				series[measurement+"_"+k+"{"+tags+"}"] = v
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func secretFile(t *testing.T, v string) string {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func testSnap() fleet.Snapshot {
	return fleet.Snapshot{Host: "host-1", At: 1767225600, Seq: 1, RelayOK: true,
		Agents: []fleet.AgentReading{{Name: "agent-a", State: fleet.StateStalled}}}
}

func TestHeartbeatToMimirInflux(t *testing.T) {
	series := map[string]string{}
	srv := mockMimir(t, "123456", "glc_test_token", series)
	defer srv.Close()
	c := fleet.HeartbeatConfig{URL: srv.URL + "/api/v1/push/influx/write", Format: "influx",
		BasicUser: "123456", PasswordFile: secretFile(t, "glc_test_token")}

	body, ct, err := encodeHeartbeat(c, testSnap())
	if err != nil {
		t.Fatal(err)
	}
	if err := sendHeartbeat(c, body, ct); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"buzzfleet_heartbeat_up{host=host-1}":                                   "1",
		"buzzfleet_agent_healthy{agent=agent-a,host=host-1}":                    "0",
		"buzzfleet_agent_state_active{agent=agent-a,host=host-1,state=stalled}": "1",
		"buzzfleet_agent_state_active{agent=agent-a,host=host-1,state=ok}":      "0",
		"buzzfleet_agents_ok{host=host-1}":                                      "0",
	} {
		if series[name] != want {
			t.Fatalf("%s = %q, want %q; got %v", name, series[name], want, series)
		}
	}
}

func TestHeartbeatAuthAndErrors(t *testing.T) {
	var gotAuth, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if gotAuth != "Bearer tok-1" {
			http.Error(w, "nope", 403)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := fleet.HeartbeatConfig{URL: srv.URL, BearerTokenFile: secretFile(t, "tok-1")}
	body, ct, _ := encodeHeartbeat(c, testSnap())
	if err := sendHeartbeat(c, body, ct); err != nil || gotType != "application/json" {
		t.Fatalf("bearer json: %v %q", err, gotType)
	}

	c.BearerTokenFile = secretFile(t, "wrong")
	err := sendHeartbeat(c, body, ct)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("a refused heartbeat must say why: %v", err)
	}

	c.BearerTokenFile = "/nonexistent/token"
	if err := sendHeartbeat(c, body, ct); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("missing token file: %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:secret@example.invalid/x":        "https://example.invalid",
		"https://example.invalid/hooks/PATHSECRET?a=b": "https://example.invalid",
		"https://example.invalid:8443/x?token=QSECRET": "https://example.invalid:8443",
		"not a url": "(heartbeat url)",
	} {
		if got := redactURL(in); got != want {
			t.Fatalf("%s: got %s", in, got)
		}
	}
}

// Webhook receivers put secrets in the path and query. None of them may
// reach the journal: not in the start line, not in the failure line.
func TestHeartbeatSecretsNeverLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close() // connection refused from here on

	cfg := &fleet.Config{Host: "host-1", HealWindowSecs: 3600, Heartbeat: fleet.HeartbeatConfig{
		URL: strings.Replace(dead, "http://", "http://u:PWSECRET@", 1) + "/hooks/PATHSECRET?token=QUERYSECRET"}}
	out := capture(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		heartbeatLoop(ctx, host.New(t.TempDir()), cfg, &fleetView{})
	})
	if !strings.Contains(out, "heartbeat failed") {
		t.Fatalf("expected a failure line, got:\n%s", out)
	}
	for _, secret := range []string{"PWSECRET", "PATHSECRET", "QUERYSECRET"} {
		if strings.Contains(out, secret) {
			t.Fatalf("%s leaked into the log:\n%s", secret, out)
		}
	}
}
