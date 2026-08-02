package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
	"github.com/nbd-wtf/go-nostr"
)

// Stand the supervisor up: its own identity, community membership, config, unit.
//
// It needs to be a separate member rather than one of your agents, for the same
// reason two harnesses cannot share a key — the relay would see one identity and
// both would answer. And a separate process, because the whole job is reporting
// on agents that have stopped answering.
func cmdInit(h *host.Host, args []string) {
	flags := parseFlags(args)
	cfg, err := loadConfig(h)
	fresh := err != nil
	if fresh {
		cfg = (&fleet.Config{}).WithDefaults()
	}

	if v := flags["relay"]; v != "" {
		cfg.Relay = v
	}
	if cfg.Relay == "" {
		// Copy it from an agent already on this host rather than asking twice.
		for _, n := range h.Agents() {
			if ac, err := h.AgentConfig(n); err == nil && ac.Relay != "" {
				cfg.Relay = ac.Relay
				break
			}
		}
	}
	if cfg.Relay == "" {
		die("no --relay given and no existing agent to copy it from")
	}
	if v := flags["name"]; v != "" {
		cfg.Name = v
	} else if cfg.Name == "fleet" {
		hn, _ := os.Hostname()
		cfg.Name = "fleet-" + hn
	}
	if cfg.Broadcast == "" {
		cfg.Broadcast = "fleet"
	}
	if cfg.Host == "" {
		cfg.Host, _ = os.Hostname()
	}

	if fresh {
		invite := flags["invite"]
		if invite == "" {
			die("first-time setup needs --invite <code> so the supervisor can join the community")
		}
		sk := nostr.GeneratePrivateKey()
		pk, err := nostr.GetPublicKey(sk)
		if err != nil {
			die("keygen failed: %v", err)
		}
		cfg.PrivateKey, cfg.PublicKey = sk, pk
		if err := joinCommunity(cfg.Relay, invite, sk); err != nil {
			die("supervisor could not join the community: %v", err)
		}
		fmt.Printf("  identity %s… joined community\n", pk[:16])
	}

	if v := flags["channel"]; v != "" {
		cfg.Channel = v
	}
	if cfg.Channel == "" {
		chans, err := h.Channels(cfg.PrivateKey, cfg.Relay)
		if err != nil || len(chans) == 0 {
			die("supervisor can see no channels — pass --channel <uuid>")
		}
		cfg.Channel = chans[0].ID
		fmt.Printf("  control channel: %s (%s)\n", chans[0].Name, cfg.Channel)
	}

	// Work out who the owner is rather than making someone hunt for their own
	// pubkey. The relay already knows: channel members carry roles.
	if v := flags["owner"]; v != "" {
		cfg.Owner = v
	}
	if cfg.Owner == "" {
		members, err := h.Members(cfg.PrivateKey, cfg.Relay, cfg.Channel)
		if err == nil {
			var owners []string
			for _, m := range members {
				if m.Role == "owner" {
					owners = append(owners, m.Pubkey)
				}
			}
			switch len(owners) {
			case 1:
				cfg.Owner = owners[0]
				fmt.Printf("  owner detected: %s…\n", cfg.Owner[:16])
			case 0:
			default:
				die("this channel has %d owners — pass --owner <hex> to say which one commands the fleet:\n    %s",
					len(owners), strings.Join(owners, "\n    "))
			}
		}
	}
	if len(cfg.Owner) != 64 {
		die("could not determine the owner — pass --owner <your 64-char hex pubkey>")
	}

	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath(h), append(b, '\n'), 0o600); err != nil {
		die("could not write %s: %v", configPath(h), err)
	}

	// A display name only affects how Buzz shows it; the handle stays the thing
	// you @mention, because that is what the parser matches on.
	display := flags["display-name"]
	if display == "" {
		display = cfg.Name
	}
	if _, err := h.Buzz(cfg.PrivateKey, cfg.Relay, "users", "set-profile",
		"--name", display, "--about", "Fleet supervisor on "+cfg.Host); err == nil {
		fmt.Printf("  profile published as %q\n", display)
	}

	h.Systemctl("enable", "--now", "buzzfleetd.service")
	fmt.Printf("  buzzfleetd: %s\n", h.Systemctl("is-active", "buzzfleetd.service"))
	fmt.Printf("\nSay \"@%s status\" in that channel, from any Buzz client.\n", cfg.Name)
}

func parseFlags(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") && i+1 < len(args) {
			out[strings.TrimPrefix(args[i], "--")] = args[i+1]
			i++
		}
	}
	return out
}

// ── joining a community ───────────────────────────────────────────────────────

// Hosted relays gate joining behind a policy, so accept it for a receipt first,
// then claim the invite NIP-98-signed. No CLI ships this: the redeem flow only
// exists in the desktop onboarding UI.
func joinCommunity(relay, invite, secret string) error {
	base := strings.TrimRight(host.HTTPRelay(relay), "/")

	receipt := ""
	if version, err := joinPolicyVersion(base); err == nil && version != "" {
		body, _ := json.Marshal(map[string]any{
			"code": invite, "policy_version": version, "age_confirmed": true,
		})
		resp, err := http.Post(base+"/api/invites/accept-policy", "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("accept-policy: %w", err)
		}
		defer resp.Body.Close()
		var r struct {
			Receipt string `json:"receipt"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return fmt.Errorf("accept-policy response: %w", err)
		}
		receipt = r.Receipt
		fmt.Println("  join policy accepted")
	}

	payload := map[string]any{"code": invite}
	if receipt != "" {
		payload["policy_receipt"] = receipt
	}
	body, _ := json.Marshal(payload)
	url := base + "/api/invites/claim"

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	auth, err := nip98Header(url, body, secret)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("relay said %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	if !bytes.Contains(out, []byte("joined")) && !bytes.Contains(out, []byte("already_member")) {
		return fmt.Errorf("unexpected response: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func joinPolicyVersion(base string) (string, error) {
	resp, err := http.Get(base + "/api/join-policy")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	var r struct {
		Version string `json:"version"`
		Policy  struct {
			Version string `json:"version"`
		} `json:"policy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if r.Policy.Version != "" {
		return r.Policy.Version, nil
	}
	return r.Version, nil
}

// NIP-98: a signed kind 27235 carrying the exact URL, the method, and a hash of
// the body. The relay requires the payload tag for signed POSTs and checks the
// u tag against the request URL, so both are fixed before signing.
func nip98Header(url string, body []byte, secret string) (string, error) {
	sum := sha256.Sum256(body)
	ev := nostr.Event{
		Kind:      27235,
		CreatedAt: nostr.Timestamp(time.Now().Unix()),
		Tags: nostr.Tags{
			{"u", url},
			{"method", "POST"},
			{"payload", hex.EncodeToString(sum[:])},
			{"nonce", uuid.NewString()},
		},
	}
	if err := ev.Sign(secret); err != nil {
		return "", err
	}
	j, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(j), nil
}
