package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// fleetView is the latest health pass, shared with the heartbeat. The
// heartbeat never assesses agents itself: that talks to the relay, and the
// heartbeat must keep going when the relay does not.
type fleetView struct {
	mu      sync.Mutex
	at      time.Time
	relayOK bool
	agents  []fleet.AgentReading
}

func (v *fleetView) update(rows []fleet.Assessment, h *host.Host, heals map[string][]int64, now time.Time, window int64) {
	relayOK := true
	readings := make([]fleet.AgentReading, 0, len(rows))
	for _, r := range rows {
		if r.State == fleet.StateUnknown && r.Detail == fleet.DetailRelayUnreachable {
			relayOK = false
		}
		var up int64
		if r.Unit == "active" {
			if t := h.UpSince(r.Name); !t.IsZero() {
				up = int64(now.Sub(t).Seconds())
			}
		}
		readings = append(readings, fleet.AgentReading{Name: r.Name, State: r.State, Detail: r.Detail, UpSecs: up,
			Restarts: fleet.HealsInWindow(heals, r.Name, now.Unix(), window)})
	}
	v.mu.Lock()
	v.at, v.relayOK, v.agents = now, relayOK, readings
	v.mu.Unlock()
}

func (v *fleetView) snapshot(h *host.Host, cfg *fleet.Config, seq uint64, started, now time.Time) fleet.Snapshot {
	v.mu.Lock()
	at, relayOK, agents := v.at, v.relayOK, append([]fleet.AgentReading(nil), v.agents...)
	v.mu.Unlock()
	if at.IsZero() {
		at = started // nothing assessed yet: as stale as the supervisor is old
	}
	return fleet.Snapshot{
		Host: cfg.Host, At: now.Unix(), Seq: seq, Version: version,
		Metrics:     host.ReadMetrics(h.FleetHome),
		AssessedAgo: int64(now.Sub(at).Seconds()), RelayOK: relayOK, Agents: agents,
	}
}

func encodeHeartbeat(c fleet.HeartbeatConfig, s fleet.Snapshot) ([]byte, string, error) {
	if c.Format == "influx" {
		return fleet.EncodeInflux(s), "text/plain; charset=utf-8", nil
	}
	b, err := fleet.EncodeJSON(s)
	return b, "application/json", err
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

var heartbeatClient = &http.Client{Timeout: 15 * time.Second}

// sendHeartbeat posts one body. Secrets are read on every send, so rotating a
// token is a file write, not a supervisor restart.
func sendHeartbeat(c fleet.HeartbeatConfig, body []byte, contentType string) error {
	req, err := http.NewRequest(http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "buzzfleet/"+version)
	switch {
	case c.BasicUser != "":
		pw, err := readSecret(c.PasswordFile)
		if err != nil {
			return fmt.Errorf("heartbeat password: %w", err)
		}
		req.SetBasicAuth(c.BasicUser, pw)
	case c.BearerTokenFile != "":
		tok, err := readSecret(c.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("heartbeat token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := heartbeatClient.Do(req)
	if err != nil {
		// url.Error quotes the whole URL, and webhook secrets often live in
		// its path or query. Keep the cause, drop the URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s %s: %w", ue.Op, redactURL(c.URL), ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var msg [512]byte
		n, _ := resp.Body.Read(msg[:])
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg[:n])))
	}
	return nil
}

// heartbeatLoop pushes one heartbeat at start, proving the supervisor came up,
// then one per interval. Failures are logged when they start, every 30th time
// while they last, and when they end, so a dead endpoint does not fill the
// journal.
func heartbeatLoop(ctx context.Context, h *host.Host, cfg *fleet.Config, view *fleetView) {
	c := cfg.Heartbeat
	if !c.Enabled() {
		return
	}
	if err := c.Validate(); err != nil {
		logf("heartbeat disabled: %v", err)
		return
	}
	started := time.Now()
	logf("heartbeat: %s every %ds to %s", formatName(c), c.Interval(), redactURL(c.URL))
	var seq uint64
	failing := 0
	beat := func() {
		seq++
		body, ct, err := encodeHeartbeat(c, view.snapshot(h, cfg, seq, started, time.Now()))
		if err == nil {
			err = sendHeartbeat(c, body, ct)
		}
		switch {
		case err != nil:
			failing++
			if failing == 1 || failing%30 == 0 {
				logf("heartbeat failed (%d in a row): %v", failing, err)
			}
		case failing > 0:
			logf("heartbeat delivered again after %d failure(s)", failing)
			failing = 0
		}
	}
	beat()
	t := time.NewTicker(time.Duration(c.Interval()) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beat()
		}
	}
}

func formatName(c fleet.HeartbeatConfig) string {
	if c.Format == "" {
		return "json"
	}
	return c.Format
}

// redactURL reduces a URL to scheme and host for logs. Webhook receivers put
// secrets in userinfo, the path (/hooks/<token>) or the query (?token=).
func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "(heartbeat url)"
	}
	return p.Scheme + "://" + p.Host
}

// cmdHeartbeat builds one heartbeat from a fresh health pass and prints it,
// or with --send posts it, so a target can be checked before relying on it.
func cmdHeartbeat(h *host.Host, args []string) {
	cfg := mustConfig(h)
	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}
	send := len(args) > 0 && args[0] == "--send"
	c := cfg.Heartbeat
	if err := c.Validate(); err != nil {
		die("%v", err)
	}
	view := &fleetView{}
	view.update(assessAll(h, cfg), h, nil, time.Now(), int64(cfg.HealWindowSecs))
	body, ct, err := encodeHeartbeat(c, view.snapshot(h, cfg, 1, time.Now(), time.Now()))
	if err != nil {
		die("%v", err)
	}
	if !send {
		fmt.Print(string(body))
		if !bytes.HasSuffix(body, []byte("\n")) {
			fmt.Println()
		}
		return
	}
	if !c.Enabled() {
		die("no heartbeat url in %s", configPath(h))
	}
	if err := sendHeartbeat(c, body, ct); err != nil {
		die("heartbeat to %s failed: %v", redactURL(c.URL), err)
	}
	fmt.Printf("heartbeat delivered to %s (%s, %d bytes)\n", redactURL(c.URL), formatName(c), len(body))
}
