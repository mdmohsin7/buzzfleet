package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"
)

// Reading and writing an agent's Buzz memory on one community's relay, as the
// agent. The relay serves engrams only to their author or their owner, and
// wants NIP-42 auth first.

// How long the relay's AUTH challenge is given to arrive before it is signed;
// signing an empty challenge gets the socket refused for good.
const engramAuthSettle = 750 * time.Millisecond

// connectAs opens an authenticated connection to a relay as secret (hex).
func connectAs(ctx context.Context, relay, secret string) (*nostr.Relay, error) {
	r, err := nostr.RelayConnect(ctx, WSRelay(relay))
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	select {
	case <-time.After(engramAuthSettle):
	case <-ctx.Done():
		r.Close()
		return nil, ctx.Err()
	}
	if err := r.Auth(ctx, func(ev *nostr.Event) error { return ev.Sign(secret) }); err != nil {
		r.Close()
		return nil, fmt.Errorf("auth: %w", err)
	}
	return r, nil
}

// FetchHeads reads every engram of (agent, owner) on a relay and keeps the
// valid ones, per NIP-AE: signature, exactly one d and one p tag, a body that
// decrypts and parses, and a slug that re-derives to the event's d tag. It
// returns how many events the relay sent, so a capped listing can be seen.
func FetchHeads(ctx context.Context, relay, secret, owner string) ([]fleet.Head, int, error) {
	// Tags are compared exactly, and relays treat hex case-insensitively:
	// an owner written in upper case would otherwise match nothing.
	owner = strings.ToLower(owner)
	agent, err := nostr.GetPublicKey(secret)
	if err != nil {
		return nil, 0, err
	}
	key, err := nip44.GenerateConversationKey(owner, secret)
	if err != nil {
		return nil, 0, err
	}
	r, err := connectAs(ctx, relay, secret)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	evs, err := paginate(func(f nostr.Filter) ([]*nostr.Event, error) { return r.QuerySync(ctx, f) }, nostr.Filter{
		Kinds: []int{fleet.EngramKind}, Authors: []string{agent},
		Tags: nostr.TagMap{"p": []string{owner}},
	}, engramPage)
	if err != nil {
		return nil, 0, err
	}
	var out []fleet.Head
	for _, ev := range evs {
		if h, ok := validEngram(ev, agent, owner, key); ok {
			out = append(out, h)
		}
	}
	return out, len(evs), nil
}

// A Buzz relay caps every query at a page (1000 events), whatever limit is
// asked for, and says nothing when it does. So listing is paged with
// `until`, newest first, until a page comes back short. Pages overlap on
// their boundary second and are de-duplicated by id; a page that adds
// nothing new while still full means more than a page's worth of events
// share one second, and the listing cannot be completed.
const engramPage = 500

func paginate(query func(nostr.Filter) ([]*nostr.Event, error), base nostr.Filter, page int) ([]*nostr.Event, error) {
	seen := map[string]bool{}
	var out []*nostr.Event
	f := base
	f.Limit = page
	for {
		evs, err := query(f)
		if err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}
		oldest := nostr.Timestamp(0)
		added := 0
		for _, ev := range evs {
			if !seen[ev.ID] {
				seen[ev.ID] = true
				out = append(out, ev)
				added++
			}
			if oldest == 0 || ev.CreatedAt < oldest {
				oldest = ev.CreatedAt
			}
		}
		if len(evs) < page {
			return out, nil
		}
		if added == 0 {
			return nil, fmt.Errorf("more than %d memory events share one second; the listing cannot be completed, so nothing was synced", page)
		}
		until := oldest
		f.Until = &until
	}
}

func validEngram(ev *nostr.Event, agent, owner string, key [32]byte) (fleet.Head, bool) {
	if ev.Kind != fleet.EngramKind || ev.PubKey != agent {
		return fleet.Head{}, false
	}
	if ok, err := ev.CheckSignature(); err != nil || !ok {
		return fleet.Head{}, false
	}
	var d, p []string
	for _, t := range ev.Tags {
		if len(t) >= 2 && t[0] == "d" {
			d = append(d, t[1])
		}
		if len(t) >= 2 && t[0] == "p" {
			p = append(p, t[1])
		}
	}
	if len(d) != 1 || len(p) != 1 || p[0] != owner {
		return fleet.Head{}, false
	}
	plain, err := nip44.Decrypt(ev.Content, key)
	if err != nil {
		return fleet.Head{}, false
	}
	body, ok := fleet.ParseEngramBody(plain)
	if !ok || fleet.DTag(key, body.Slug) != d[0] {
		return fleet.Head{}, false
	}
	raw, _ := json.Marshal(ev)
	return fleet.Head{
		D: d[0], ID: ev.ID, CreatedAt: int64(ev.CreatedAt), Slug: body.Slug,
		Tombstone: body.Slug != "core" && body.Value == nil, Plain: plain, Event: raw,
	}, true
}

// PublishEngram sends a signed engram, byte for byte as it was signed.
func PublishEngram(ctx context.Context, relay, secret string, raw json.RawMessage) error {
	var ev nostr.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return err
	}
	r, err := connectAs(ctx, relay, secret)
	if err != nil {
		return err
	}
	defer r.Close()
	return r.Publish(ctx, ev)
}

// ResignEngram makes a new event carrying the same body as head, timestamped
// after notBefore, so it beats the head it has to replace. Only resolving a
// conflict for the older side needs this; everything else is copied verbatim.
func ResignEngram(head fleet.Head, secret, owner string, notBefore int64) (json.RawMessage, error) {
	key, err := nip44.GenerateConversationKey(owner, secret)
	if err != nil {
		return nil, err
	}
	content, err := nip44.Encrypt(head.Plain, key)
	if err != nil {
		return nil, err
	}
	at := time.Now().Unix()
	if at <= notBefore {
		at = notBefore + 1
	}
	ev := nostr.Event{
		Kind: fleet.EngramKind, CreatedAt: nostr.Timestamp(at), Content: content,
		Tags: nostr.Tags{{"d", head.D}, {"p", owner}},
	}
	if err := ev.Sign(secret); err != nil {
		return nil, err
	}
	return json.Marshal(ev)
}
