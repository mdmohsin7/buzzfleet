package fleet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"sort"
	"strings"
)

// ── memory sync ───────────────────────────────────────────────────────────────
//
// Buzz memory (NIP-AE engrams) is a set of addressable kind:30174 events signed
// by the agent and encrypted to it and its owner. It belongs to the (agent,
// owner) pair, not to a community; a community has it only in the sense of
// holding the events on its relay. With the same owner everywhere, the same
// signed events are valid on every relay, so syncing is copying events
// verbatim: nothing is re-signed or re-encrypted, and each side ends up with a
// byte-identical head.
//
// Last-writer-wins by created_at is what relays apply, and it would silently
// drop one side's edit when a memory changed on both since the last sync. So a
// sync remembers, per pair of communities, the head it last left both sides
// on, and a memory that moved on both sides since is a conflict to resolve,
// not a race to lose.

// EngramKind is NIP-AE's addressable kind.
const EngramKind = 30174

var slugRe = regexp.MustCompile(`^mem/[a-z0-9][a-z0-9_-]{0,63}(/[a-z0-9][a-z0-9_-]{0,63})*$`)

// ValidSlug is NIP-AE's slug grammar: "core", or mem/… path segments.
func ValidSlug(s string) bool {
	return s == "core" || (len(s) <= 255 && slugRe.MatchString(s))
}

// DTag derives a slug's d tag under the agent–owner conversation key, as
// NIP-AE specifies: HMAC-SHA256(K_c, "agent-memory/v1/d-tag" 0x00 slug).
func DTag(conversationKey [32]byte, slug string) string {
	m := hmac.New(sha256.New, conversationKey[:])
	m.Write([]byte("agent-memory/v1/d-tag"))
	m.Write([]byte{0})
	m.Write([]byte(slug))
	return hex.EncodeToString(m.Sum(nil))
}

// EngramBody is the decrypted content of an engram.
type EngramBody struct {
	Slug    string           `json:"slug"`
	Value   *string          `json:"value"`             // memory; nil is a tombstone
	Profile *string          `json:"profile,omitempty"` // core
	Extra   *json.RawMessage `json:"-"`
}

// ParseEngramBody reads a decrypted body. NIP-AE requires a body with
// duplicate member names to be rejected, because parsers that keep the first
// or the last would disagree about which head is current.
func ParseEngramBody(plaintext string) (EngramBody, bool) {
	if hasDuplicateKeys([]byte(plaintext)) {
		return EngramBody{}, false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(plaintext), &raw) != nil {
		return EngramBody{}, false
	}
	var b EngramBody
	if json.Unmarshal(raw["slug"], &b.Slug) != nil || !ValidSlug(b.Slug) {
		return EngramBody{}, false
	}
	if b.Slug == "core" {
		var p string
		if json.Unmarshal(raw["profile"], &p) != nil {
			return EngramBody{}, false
		}
		b.Profile = &p
		return b, true
	}
	v, ok := raw["value"]
	if !ok {
		return EngramBody{}, false
	}
	if string(v) != "null" {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return EngramBody{}, false
		}
		b.Value = &s
	}
	return b, true
}

func hasDuplicateKeys(b []byte) bool {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	type frame struct {
		obj, wantKey bool
		seen         map[string]bool
	}
	var st []*frame
	// done marks the end of a value inside its parent.
	done := func() bool {
		if len(st) == 0 {
			return true
		}
		if top := st[len(st)-1]; top.obj {
			top.wantKey = true
		}
		return false
	}
	for {
		t, err := dec.Token()
		if err != nil {
			return false
		}
		if n := len(st); n > 0 && st[n-1].obj && st[n-1].wantKey {
			if d, ok := t.(json.Delim); ok && d == '}' {
				st = st[:n-1]
				if done() {
					return false
				}
				continue
			}
			k, _ := t.(string)
			if st[n-1].seen[k] {
				return true
			}
			st[n-1].seen[k] = true
			st[n-1].wantKey = false
			continue
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{':
				st = append(st, &frame{obj: true, wantKey: true, seen: map[string]bool{}})
				continue
			case '[':
				st = append(st, &frame{})
				continue
			case ']':
				st = st[:len(st)-1]
			}
		}
		if done() {
			return false
		}
	}
}

// Head is the current engram for one d tag on one relay.
type Head struct {
	D         string
	ID        string
	CreatedAt int64
	Slug      string
	Tombstone bool
	Plain     string          // the decrypted body, for conflict reports and re-publishing
	Event     json.RawMessage // the signed event, copied verbatim
}

// Newer reports whether a beats b under NIP-AE head selection: greater
// created_at, ties broken by the lowest event id.
func Newer(a, b Head) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID < b.ID
}

// Heads keeps the current head per d tag.
func Heads(all []Head) map[string]Head {
	out := map[string]Head{}
	for _, h := range all {
		if cur, ok := out[h.D]; !ok || Newer(h, cur) {
			out[h.D] = h
		}
	}
	return out
}

// SyncOptions shapes a sync between two communities.
type SyncOptions struct {
	Both        bool              // also copy "to" changes back to "from"
	Exclude     []string          // slug globs never read across or written
	IncludeCore bool              // core is the agent's identity per community; off by default
	Resolve     map[string]string // slug -> "from" or "to", for conflicts
}

type SyncKind string

const (
	ActInSync   SyncKind = "in-sync"
	ActCopyTo   SyncKind = "copy-to"   // from's head onto to
	ActCopyFrom SyncKind = "copy-from" // to's head onto from
	ActConflict SyncKind = "conflict"
	ActExcluded SyncKind = "excluded"
	ActKeepTo   SyncKind = "to-newer" // one-way: to changed and from did not; left alone
	ActOnlyTo   SyncKind = "only-to"  // one-way: exists only on to; left alone
)

// SyncAction is what to do about one memory.
type SyncAction struct {
	Kind SyncKind
	D    string
	Slug string
	From *Head
	To   *Head
	// Fresh: the side being written has the newer head, so the chosen head
	// must be re-published under a later timestamp to win (resolve only).
	Fresh bool
}

// Excluded reports whether a slug stays in its community.
func Excluded(slug string, o SyncOptions) bool {
	if slug == "core" && !o.IncludeCore {
		return true
	}
	for _, g := range o.Exclude {
		if GlobMatch(g, slug) {
			return true
		}
	}
	return false
}

// GlobMatch matches a slug against a glob where * is one path segment and **
// any number of them: mem/private/** covers everything under mem/private.
func GlobMatch(glob, slug string) bool {
	return matchSegs(strings.Split(glob, "/"), strings.Split(slug, "/"))
}

func matchSegs(g, s []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(s); i++ {
				if matchSegs(g[1:], s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], s[0]); !ok {
			return false
		}
		g, s = g[1:], s[1:]
	}
	return len(s) == 0
}

// PlanSync decides, per memory, what a sync between two communities does.
// last is what the previous sync of this pair left both sides on (d -> event
// id). It returns the actions and the record for the next sync: for each d
// both sides will share after the copies, that shared head's id.
func PlanSync(from, to map[string]Head, last map[string]string, o SyncOptions) ([]SyncAction, map[string]string) {
	ds := map[string]bool{}
	for d := range from {
		ds[d] = true
	}
	for d := range to {
		ds[d] = true
	}
	keys := make([]string, 0, len(ds))
	for d := range ds {
		keys = append(keys, d)
	}
	sort.Strings(keys)

	next := map[string]string{}
	var acts []SyncAction
	for _, d := range keys {
		a, inA := from[d]
		b, inB := to[d]
		act := SyncAction{D: d}
		if inA {
			act.From, act.Slug = &a, a.Slug
		}
		if inB {
			act.To, act.Slug = &b, b.Slug
		}
		if Excluded(act.Slug, o) {
			act.Kind = ActExcluded
			acts = append(acts, act)
			continue
		}
		switch {
		case inA && inB && a.ID == b.ID:
			act.Kind = ActInSync
			next[d] = a.ID
		case inA && !inB:
			act.Kind = ActCopyTo
			next[d] = a.ID
		case !inA && inB:
			if o.Both {
				act.Kind = ActCopyFrom
				next[d] = b.ID
			} else {
				act.Kind = ActOnlyTo
			}
		default:
			l := last[d]
			switch r := o.Resolve[act.Slug]; {
			case r == "from":
				act.Kind, act.Fresh = ActCopyTo, Newer(b, a)
				next[d] = "" // a re-published head has a new id; read it next time
			case r == "to":
				act.Kind, act.Fresh = ActCopyFrom, Newer(a, b)
				next[d] = ""
			case l != "" && l == b.ID:
				// to is where it was; from moved on.
				act.Kind = ActCopyTo
				next[d] = a.ID
			case l != "" && l == a.ID:
				// from is where it was; to moved on.
				if o.Both {
					act.Kind = ActCopyFrom
					next[d] = b.ID
				} else {
					act.Kind = ActKeepTo
					next[d] = l
				}
			default:
				// Both moved since the last sync, or this is the first sync
				// and they already differ: nobody can say which edit to keep.
				act.Kind = ActConflict
				if l != "" {
					next[d] = l
				}
			}
		}
		for k, v := range next {
			if v == "" {
				delete(next, k)
			}
		}
		acts = append(acts, act)
	}
	return acts, next
}
