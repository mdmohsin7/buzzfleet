package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/mdmohsin7/buzzfleet/internal/host"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// Buzz memory follows the agent into every community it is in: the same
// signed engrams, copied between the communities' relays. See
// docs/design/multi-community.md, section 4a.

func cmdMemory(h *host.Host, args []string) {
	if len(args) == 0 {
		die("usage: buzzfleet memory sync|auto ...")
	}
	switch args[0] {
	case "sync":
		cmdMemorySync(h, args[1:])
	case "auto":
		cmdMemoryAuto(h, args[1:])
	default:
		die("unknown memory command %q: sync or auto", args[0])
	}
}

const memorySyncUsage = `usage: buzzfleet memory sync <agent> [--from <community>] [--to <community>] [--both]
          [--exclude <slug-glob>]... [--include-core] [--dry-run] [--resolve <slug>=from|to]...`

// memFlags reads flags that may repeat.
func memFlags(args []string) (map[string][]string, []string) {
	out := map[string][]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		k := strings.TrimPrefix(a, "--")
		switch k {
		case "both", "include-core", "dry-run", "off":
			out[k] = append(out[k], "")
		default:
			if i+1 >= len(args) {
				die("--%s needs a value", k)
			}
			out[k] = append(out[k], args[i+1])
			i++
		}
	}
	return out, pos
}

func hexSecret(s string) (string, error) {
	if strings.HasPrefix(s, "nsec") {
		_, v, err := nip19.Decode(s)
		if err != nil {
			return "", err
		}
		return v.(string), nil
	}
	return s, nil
}

// syncRecordPath is what the last sync of a pair left both sides on: decrypted
// slugs never go in it, only d tags and event ids, but it lives with the rest
// of the agent's private files all the same.
func syncRecordPath(h *host.Host, agent, from, to string) string {
	return filepath.Join(h.AgentsDir(), agent, "memory-sync", from+"--"+to+".json")
}

func conflictDir(h *host.Host, agent, from, to string) string {
	return filepath.Join(h.AgentsDir(), agent, "memory-conflicts", from+"--"+to)
}

func readRecord(p string) map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// syncResult is what one pair's sync did.
type syncResult struct {
	Copied, Conflicts, Excluded, InSync int
	Lines                               []string
}

// syncPair syncs one pair of the agent's communities. The caller holds the
// agent's turn, so no turn of the agent is writing memory meanwhile.
func syncPair(ctx context.Context, h *host.Host, c *host.AgentConfig, from, to string, o fleet.SyncOptions, dry bool) (syncResult, error) {
	var res syncResult
	mf, okf := c.In(from)
	mt, okt := c.In(to)
	if !okf || !okt {
		return res, fmt.Errorf("%s is not in both %q and %q", c.Name, from, to)
	}
	if mf.Owner == "" || mf.Owner != mt.Owner {
		// A different owner means a different memory namespace: copying would
		// mean re-encrypting for that owner, who could then read all of it.
		return res, fmt.Errorf("%s has different owners in %s and %s; its memory is not shared between them", c.Name, from, to)
	}
	secret, err := hexSecret(h.AgentSecret(c.Name))
	if err != nil || secret == "" {
		return res, fmt.Errorf("no usable identity for %s", c.Name)
	}
	fetch := func(relay string) (map[string]fleet.Head, error) {
		hs, n, err := host.FetchHeads(ctx, relay, secret, mf.Owner)
		if err != nil {
			return nil, err
		}
		if n >= 10000 {
			return nil, fmt.Errorf("the relay returned %d events, its limit: the listing may be incomplete, so nothing was synced", n)
		}
		return fleet.Heads(hs), nil
	}
	fh, err := fetch(mf.Relay)
	if err != nil {
		return res, fmt.Errorf("%s: %w", from, err)
	}
	th, err := fetch(mt.Relay)
	if err != nil {
		return res, fmt.Errorf("%s: %w", to, err)
	}
	recPath := syncRecordPath(h, c.Name, from, to)
	acts, next := fleet.PlanSync(fh, th, readRecord(recPath), o)

	say := func(format string, a ...any) { res.Lines = append(res.Lines, fmt.Sprintf(format, a...)) }
	var conflicts []fleet.SyncAction
	for _, a := range acts {
		switch a.Kind {
		case fleet.ActInSync:
			res.InSync++
		case fleet.ActExcluded:
			res.Excluded++
		case fleet.ActOnlyTo:
			say("  %s: only in %s (one-way sync; left there)", a.Slug, to)
		case fleet.ActKeepTo:
			say("  %s: changed in %s since the last sync, not in %s; left as is (one-way)", a.Slug, to, from)
		case fleet.ActConflict:
			res.Conflicts++
			conflicts = append(conflicts, a)
			say("  %s: CONFLICT, changed in both since the last sync", a.Slug)
		case fleet.ActCopyTo, fleet.ActCopyFrom:
			src, other := a.From, a.To
			srcRelay, dst, dstName := mf.Relay, mt.Relay, to
			if a.Kind == fleet.ActCopyFrom {
				src, other = a.To, a.From
				srcRelay, dst, dstName = mt.Relay, mf.Relay, from
			}
			verb := "copy"
			if src.Tombstone {
				verb = "delete"
			}
			say("  %s: %s to %s", a.Slug, verb, dstName)
			if dry {
				continue
			}
			raw := src.Event
			if a.Fresh && other != nil {
				// Resolved for the older side: publish it again under a later
				// timestamp, on both sides, so it beats the other head.
				if raw, err = host.ResignEngram(*src, secret, mf.Owner, other.CreatedAt); err != nil {
					return res, err
				}
				if err := host.PublishEngram(ctx, srcRelay, secret, raw); err != nil {
					return res, fmt.Errorf("%s: %w", a.Slug, err)
				}
			}
			if err := host.PublishEngram(ctx, dst, secret, raw); err != nil {
				return res, fmt.Errorf("%s to %s: %w", a.Slug, dstName, err)
			}
			res.Copied++
		}
	}
	if dry {
		return res, nil
	}
	// Record what both sides now share, read back rather than assumed: a
	// relay may have refused a write, and a re-published head has a new id.
	fh2, err := fetch(mf.Relay)
	if err == nil {
		if th2, err2 := fetch(mt.Relay); err2 == nil {
			for d, a := range fh2 {
				if b, ok := th2[d]; ok && a.ID == b.ID {
					next[d] = a.ID
				}
			}
		}
	}
	rec, _ := json.MarshalIndent(next, "", "  ")
	if err := writePrivate(recPath, rec); err != nil {
		return res, err
	}
	// Conflicts hold decrypted memory: private files, one per slug.
	for _, a := range conflicts {
		body, _ := json.MarshalIndent(map[string]any{
			"slug":    a.Slug,
			from:      map[string]any{"event": a.From.ID, "created_at": a.From.CreatedAt, "body": json.RawMessage(a.From.Plain)},
			to:        map[string]any{"event": a.To.ID, "created_at": a.To.CreatedAt, "body": json.RawMessage(a.To.Plain)},
			"resolve": fmt.Sprintf("buzzfleet memory sync %s --from %s --to %s --resolve %s=from|to", c.Name, from, to, a.Slug),
		}, "", "  ")
		_ = writePrivate(filepath.Join(conflictDir(h, c.Name, from, to), strings.ReplaceAll(a.Slug, "/", "__")+".json"), body)
	}
	return res, nil
}

func cmdMemorySync(h *host.Host, args []string) {
	f, pos := memFlags(args)
	if len(pos) != 1 {
		die(memorySyncUsage)
	}
	c, err := h.AgentConfig(pos[0])
	if err != nil {
		die("%v", err)
	}
	if len(c.Communities) == 0 {
		die("%s is in one community; there is nothing to sync its memory with", c.Name)
	}
	from := fleet.MainCommunity
	if v := f["from"]; len(v) > 0 {
		from = v[0]
	}
	var tos []string
	if v := f["to"]; len(v) > 0 {
		tos = v
	} else {
		for _, hn := range c.Harnesses() {
			if hn.Community != from {
				tos = append(tos, hn.Community)
			}
		}
	}
	o := fleet.SyncOptions{Both: len(f["both"]) > 0, Exclude: f["exclude"], IncludeCore: len(f["include-core"]) > 0, Resolve: map[string]string{}}
	for _, r := range f["resolve"] {
		slug, side, ok := strings.Cut(r, "=")
		if !ok || (side != "from" && side != "to") {
			die("--resolve takes <slug>=from or <slug>=to")
		}
		o.Resolve[slug] = side
	}
	dry := len(f["dry-run"]) > 0

	unlock, err := takeTurn(h, c.Name, 60*time.Second)
	if err != nil {
		die("%v", err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	failed := false
	for _, to := range tos {
		res, err := syncPair(ctx, h, c, from, to, o, dry)
		fmt.Printf("%s: %s -> %s%s\n", c.Name, from, to, map[bool]string{true: " (dry run)", false: ""}[dry])
		for _, l := range res.Lines {
			fmt.Println(l)
		}
		if err != nil {
			fmt.Printf("  error: %v\n", err)
			failed = true
			continue
		}
		fmt.Printf("  %d copied, %d in sync, %d kept in their community, %d conflict(s)\n", res.Copied, res.InSync, res.Excluded, res.Conflicts)
		if res.Conflicts > 0 {
			fmt.Printf("  both versions of each conflict are in %s\n", conflictDir(h, c.Name, from, to))
		}
	}
	if failed {
		os.Exit(1)
	}
}

// takeTurn holds the agent's turn for a sync, so no turn of the agent writes
// memory while it is being copied. An agent in one community has no gate and
// nothing to wait for.
func takeTurn(h *host.Host, agent string, wait time.Duration) (func(), error) {
	l, err := host.OpenTurnLock(h.TurnLockPath(agent), "memory-sync")
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		ok, err := l.TryLock()
		if err != nil {
			l.Close()
			return nil, err
		}
		if ok {
			_ = l.RecordHolder(fleet.Holder{Harness: agent + "--memory-sync", PID: os.Getpid(), Since: time.Now().Unix()})
			return func() { _ = l.Unlock(); _ = l.Close() }, nil
		}
		if time.Now().After(deadline) {
			l.Close()
			return nil, fmt.Errorf("%s is mid-turn; try again when it is idle", agent)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ── periodic sync ─────────────────────────────────────────────────────────────

// MemorySync is the agent.json setting for keeping memory in step on its own.
type memorySyncConfig struct {
	EveryMins   int      `json:"everyMins"`
	Exclude     []string `json:"exclude,omitempty"`
	IncludeCore bool     `json:"includeCore,omitempty"`
}

func readMemorySync(h *host.Host, agent string) *memorySyncConfig {
	raw, err := h.RawAgentConfig(agent)
	if err != nil {
		return nil
	}
	var doc struct {
		MemorySync *memorySyncConfig `json:"memorySync"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.MemorySync == nil || doc.MemorySync.EveryMins <= 0 {
		return nil
	}
	return doc.MemorySync
}

func cmdMemoryAuto(h *host.Host, args []string) {
	f, pos := memFlags(args)
	if len(pos) != 1 {
		die("usage: buzzfleet memory auto <agent> --every <minutes> [--exclude <slug-glob>]... [--include-core] | --off")
	}
	agent := pos[0]
	c, err := h.AgentConfig(agent)
	if err != nil {
		die("%v", err)
	}
	raw, _ := h.RawAgentConfig(agent)
	var out []byte
	if len(f["off"]) > 0 {
		if out, err = fleet.DeleteTopLevelKey(raw, "memorySync"); err != nil {
			die("%v", err)
		}
	} else {
		if len(c.Communities) == 0 {
			die("%s is in one community; there is nothing to sync its memory with", agent)
		}
		n := 0
		if v := f["every"]; len(v) > 0 {
			n, _ = strconv.Atoi(v[0])
		}
		if n < 5 {
			die("--every takes minutes, at least 5")
		}
		v, _ := json.MarshalIndent(memorySyncConfig{EveryMins: n, Exclude: f["exclude"], IncludeCore: len(f["include-core"]) > 0}, "  ", "  ")
		if out, err = fleet.SetTopLevelKey(raw, "memorySync", v); err != nil {
			die("%v", err)
		}
	}
	if err := h.WriteRawAgentConfig(agent, out); err != nil {
		die("%v", err)
	}
	if len(f["off"]) > 0 {
		fmt.Printf("%s: automatic memory sync off\n", agent)
		return
	}
	fmt.Printf("%s: memory synced both ways between all its communities every %s minutes, by the supervisor\n", agent, f["every"][0])
}

// memorySyncLoop runs each agent's periodic sync: both ways between the
// agent's main community and each other one, holding the agent's turn, and
// skipped while the agent is mid-turn. New conflicts are reported once, as a
// count: slugs can be private to a community, and the report goes to the
// control channel of only one of them.
func memorySyncLoop(ctx context.Context, h *host.Host, cfg *fleet.Config) {
	lastRun := map[string]time.Time{}
	reported := map[string]int{}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		names := h.Agents()
		sort.Strings(names)
		for _, n := range names {
			ms := readMemorySync(h, n)
			c, err := h.AgentConfig(n)
			if ms == nil || err != nil || len(c.Communities) == 0 || time.Since(lastRun[n]) < time.Duration(ms.EveryMins)*time.Minute {
				continue
			}
			unlock, err := takeTurn(h, n, 0)
			if err != nil {
				continue // mid-turn: next minute
			}
			lastRun[n] = time.Now()
			conflicts := 0
			for _, hn := range c.Harnesses()[1:] {
				sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				res, err := syncPair(sctx, h, c, fleet.MainCommunity, hn.Community,
					fleet.SyncOptions{Both: true, Exclude: ms.Exclude, IncludeCore: ms.IncludeCore}, false)
				cancel()
				if err != nil {
					logf("memory sync %s main<->%s: %v", n, hn.Community, err)
					continue
				}
				conflicts += res.Conflicts
				if res.Copied > 0 || res.Conflicts > 0 {
					logf("memory sync %s main<->%s: %d copied, %d conflict(s)", n, hn.Community, res.Copied, res.Conflicts)
				}
			}
			unlock()
			if conflicts > 0 && conflicts != reported[n] {
				say(h, cfg, fmt.Sprintf("**%s** — %d of **%s**'s memories changed in more than one community since they were last in step, so they were left alone. "+
					"Both versions are in `agents/%s/memory-conflicts/`; resolve with `buzzfleet memory sync %s --to <community> --resolve <slug>=from|to`.",
					cfg.Host, conflicts, n, n, n), "")
			}
			reported[n] = conflicts
		}
	}
}
