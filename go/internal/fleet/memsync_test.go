package fleet

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr/nip44"
)

// NIP-AE's reference vectors (docs/nips/NIP-AE.md, "Reference test vectors").
// These keys are the spec's published test keys, not anyone's.
const (
	vecSecA = "0000000000000000000000000000000000000000000000000000000000000001"
	vecPubO = "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	vecKc   = "c41c775356fd92eadc63ff5a0dc1da211b268cbea22316767095b2871ea1412d"
	vecEv1  = "AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABedgcxyfmpph68LBjCWZsTI5lb0Cbg8dIPVYVe/WVj/l4Yd8HGgzC8awyBi9bn9ClRdtd2IPsmont0jN/cajVSQhahTOwuNNwoJtZIg35aSsUzeCq4tQfd8E+fLoKomdPxjs="
)

func TestNIPAEVectors(t *testing.T) {
	k, err := nip44.GenerateConversationKey(vecPubO, vecSecA)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(k[:]) != vecKc {
		t.Fatalf("conversation key %x: the library's ECDH must use raw shared x", k)
	}
	for slug, want := range map[string]string{
		"core":                 "bdc233238ffe52e272b44cc233c8f33a2bc510b08be04495b225964283be4a90",
		"mem/example":          "72d4f9629106451505d7d341ea85bb3ebad4f654fcfd2aad100d5a35f8a85cba",
		"mem/notes/2026-05-12": "31651571a312780cfdc1f0b706b682ac9f3f51a053e8dca76fe57710bae5a4d4",
	} {
		if got := DTag(k, slug); got != want {
			t.Fatalf("d(%s) = %s", slug, got)
		}
	}
	pt, err := nip44.Decrypt(vecEv1, k)
	if err != nil || pt != `{"slug":"mem/example","value":"hello, agent memory"}` {
		t.Fatalf("decrypt event 1: %q %v", pt, err)
	}
	b, ok := ParseEngramBody(pt)
	if !ok || b.Slug != "mem/example" || b.Value == nil || *b.Value != "hello, agent memory" {
		t.Fatalf("body %+v", b)
	}
}

func TestParseEngramBody(t *testing.T) {
	if b, ok := ParseEngramBody(`{"slug":"mem/example","value":null}`); !ok || b.Value != nil {
		t.Fatal("a null value is a tombstone")
	}
	if b, ok := ParseEngramBody(`{"slug":"core","profile":"p","extra":1}`); !ok || b.Profile == nil {
		t.Fatal("core with an unknown field is fine")
	}
	for _, bad := range []string{
		`{"slug":"mem/a","value":"x","value":"y"}`,       // duplicate, NIP-AE rule 3
		`{"slug":"mem/a","value":"x","n":{"k":1,"k":2}}`, // duplicate, nested
		`{"slug":"Mem/A","value":"x"}`,                   // bad slug
		`{"slug":"mem/a"}`,                               // no value
		`{"slug":"core"}`,                                // no profile
		`{"slug":"mem/a","value":3}`,                     // wrong type
		`not json`,
	} {
		if _, ok := ParseEngramBody(bad); ok {
			t.Fatalf("%s must be invalid", bad)
		}
	}
	if _, ok := ParseEngramBody(`{"slug":"mem/a","value":"x","n":[{"k":1},{"k":2}]}`); !ok {
		t.Fatal("the same key in two array elements is not a duplicate")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := map[[2]string]bool{
		{"mem/private/**", "mem/private/x"}:   true,
		{"mem/private/**", "mem/private/x/y"}: true,
		{"mem/private/**", "mem/private"}:     true,
		{"mem/private/**", "mem/privately"}:   false,
		{"mem/*/draft", "mem/a/draft"}:        true,
		{"mem/*/draft", "mem/a/b/draft"}:      false,
		{"mem/**/draft", "mem/a/b/draft"}:     true,
		{"mem/cust-*", "mem/cust-acme"}:       true,
		{"mem/cust-*", "mem/customer"}:        false,
	}
	for c, want := range cases {
		if got := GlobMatch(c[0], c[1]); got != want {
			t.Fatalf("%s ~ %s = %v", c[0], c[1], got)
		}
	}
}

func h(d, id string, at int64, slug string) Head {
	return Head{D: d, ID: id, CreatedAt: at, Slug: slug}
}

func kinds(acts []SyncAction) string {
	var out []string
	for _, a := range acts {
		out = append(out, a.Slug+"="+string(a.Kind))
	}
	return strings.Join(out, " ")
}

func TestPlanSyncBasics(t *testing.T) {
	from := map[string]Head{
		"d1": h("d1", "e1", 10, "mem/a"),   // only in from
		"d2": h("d2", "e2", 10, "mem/b"),   // same on both
		"d3": h("d3", "e3", 10, "core"),    // core: stays
		"d4": h("d4", "e4", 10, "mem/p/x"), // excluded
	}
	to := map[string]Head{
		"d2": h("d2", "e2", 10, "mem/b"),
		"d5": h("d5", "e5", 10, "mem/c"), // only in to
		"d4": h("d4", "e9", 20, "mem/p/x"),
	}
	acts, next := PlanSync(from, to, nil, SyncOptions{Exclude: []string{"mem/p/**"}})
	if got := kinds(acts); got != "mem/a=copy-to mem/b=in-sync core=excluded mem/p/x=excluded mem/c=only-to" {
		t.Fatalf("one-way: %s", got)
	}
	if next["d1"] != "e1" || next["d2"] != "e2" || next["d4"] != "" || next["d5"] != "" {
		t.Fatalf("record: %v", next)
	}
	acts, _ = PlanSync(from, to, nil, SyncOptions{Both: true, Exclude: []string{"mem/p/**"}, IncludeCore: true})
	if got := kinds(acts); got != "mem/a=copy-to mem/b=in-sync core=copy-to mem/p/x=excluded mem/c=copy-from" {
		t.Fatalf("both ways, core included: %s", got)
	}
}

func TestPlanSyncChangesSinceLastSync(t *testing.T) {
	last := map[string]string{"d": "e0"}
	was := h("d", "e0", 10, "mem/a")
	newer := h("d", "e1", 20, "mem/a")

	// from moved on, to did not: copy, either way.
	acts, next := PlanSync(map[string]Head{"d": newer}, map[string]Head{"d": was}, last, SyncOptions{})
	if acts[0].Kind != ActCopyTo || next["d"] != "e1" {
		t.Fatalf("%v %v", acts[0].Kind, next)
	}
	// to moved on, from did not: one-way leaves it; both ways brings it back.
	acts, next = PlanSync(map[string]Head{"d": was}, map[string]Head{"d": newer}, last, SyncOptions{})
	if acts[0].Kind != ActKeepTo || next["d"] != "e0" {
		t.Fatalf("one-way must not clobber a newer edit on to: %v %v", acts[0].Kind, next)
	}
	acts, _ = PlanSync(map[string]Head{"d": was}, map[string]Head{"d": newer}, last, SyncOptions{Both: true})
	if acts[0].Kind != ActCopyFrom {
		t.Fatal(acts[0].Kind)
	}
	// A tombstone is a write like any other.
	tomb := h("d", "e2", 30, "mem/a")
	tomb.Tombstone = true
	acts, _ = PlanSync(map[string]Head{"d": tomb}, map[string]Head{"d": was}, last, SyncOptions{})
	if acts[0].Kind != ActCopyTo {
		t.Fatal("a deletion propagates")
	}
}

func TestPlanSyncConflicts(t *testing.T) {
	a := h("d", "ea", 20, "mem/a")
	b := h("d", "eb", 30, "mem/a")
	// Both moved since the last sync.
	acts, next := PlanSync(map[string]Head{"d": a}, map[string]Head{"d": b}, map[string]string{"d": "e0"}, SyncOptions{Both: true})
	if acts[0].Kind != ActConflict || next["d"] != "e0" {
		t.Fatalf("both moved: %v %v", acts[0].Kind, next)
	}
	// First sync of the pair, already different: also a conflict, not LWW.
	acts, next = PlanSync(map[string]Head{"d": a}, map[string]Head{"d": b}, nil, SyncOptions{Both: true})
	if acts[0].Kind != ActConflict || len(next) != 0 {
		t.Fatalf("first sync: %v %v", acts[0].Kind, next)
	}
	// Resolved for from, which is older: it must be re-published to win.
	acts, _ = PlanSync(map[string]Head{"d": a}, map[string]Head{"d": b}, nil, SyncOptions{Resolve: map[string]string{"mem/a": "from"}})
	if acts[0].Kind != ActCopyTo || !acts[0].Fresh {
		t.Fatalf("resolve from: %+v", acts[0])
	}
	acts, _ = PlanSync(map[string]Head{"d": a}, map[string]Head{"d": b}, nil, SyncOptions{Resolve: map[string]string{"mem/a": "to"}})
	if acts[0].Kind != ActCopyFrom || acts[0].Fresh {
		t.Fatalf("resolve to, already newer: %+v", acts[0])
	}
}

func TestHeadsPicksNIPAEHead(t *testing.T) {
	hs := Heads([]Head{h("d", "bb", 10, "mem/a"), h("d", "aa", 10, "mem/a"), h("d", "zz", 9, "mem/a")})
	if hs["d"].ID != "aa" {
		t.Fatalf("greatest created_at, then lowest id: %s", hs["d"].ID)
	}
}
