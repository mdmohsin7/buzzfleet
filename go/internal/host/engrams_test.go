package host

import (
	"fmt"
	"testing"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip11"
	"github.com/nbd-wtf/go-nostr/nip44"
)

// NIP-AE's event 1, from the spec's reference vectors (test keys only).
func vectorEvent() *nostr.Event {
	return &nostr.Event{
		ID:        "f4a594177b7aeea4fe99a09efbf74ae85f0126244f322135682c405888a38689",
		PubKey:    "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
		CreatedAt: 1700000000,
		Kind:      30174,
		Tags: nostr.Tags{
			{"d", "72d4f9629106451505d7d341ea85bb3ebad4f654fcfd2aad100d5a35f8a85cba"},
			{"p", "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"},
		},
		Content: "AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABedgcxyfmpph68LBjCWZsTI5lb0Cbg8dIPVYVe/WVj/l4Yd8HGgzC8awyBi9bn9ClRdtd2IPsmont0jN/cajVSQhahTOwuNNwoJtZIg35aSsUzeCq4tQfd8E+fLoKomdPxjs=",
		Sig:     "0a4582f0bc5995b9a010afda5984f568055988ebbe4552b4e0ec6d11aeb2b303af940f3d84726a7edd1763badb284eb3aa8457664ceba85a90d6252ed4b494cb",
	}
}

func TestValidEngramOnTheSpecVector(t *testing.T) {
	secA := "0000000000000000000000000000000000000000000000000000000000000001"
	owner := "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	key, _ := nip44.GenerateConversationKey(owner, secA)
	ev := vectorEvent()
	h, ok := validEngram(ev, ev.PubKey, owner, key)
	if !ok || h.Slug != "mem/example" || h.Tombstone || h.CreatedAt != 1700000000 || h.ID != ev.ID {
		t.Fatalf("the spec's event must validate: %+v", h)
	}
	for name, mutate := range map[string]func(*nostr.Event){
		"bad signature":   func(e *nostr.Event) { e.Sig = e.Sig[:len(e.Sig)-2] + "00" },
		"wrong owner tag": func(e *nostr.Event) { e.Tags[1][1] = e.PubKey },
		"two d tags":      func(e *nostr.Event) { e.Tags = append(e.Tags, nostr.Tag{"d", "x"}) },
		"d of another slug": func(e *nostr.Event) {
			e.Tags[0][1] = fleet.DTag(key, "mem/other")
		},
	} {
		e := vectorEvent()
		mutate(e)
		if _, ok := validEngram(e, ev.PubKey, owner, key); ok {
			t.Fatalf("%s must be invalid", name)
		}
	}
}

func TestResignBeatsTheOtherHead(t *testing.T) {
	secA := "0000000000000000000000000000000000000000000000000000000000000001"
	owner := "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	key, _ := nip44.GenerateConversationKey(owner, secA)
	h, _ := validEngram(vectorEvent(), vectorEvent().PubKey, owner, key)
	raw, err := ResignEngram(h, secA, owner, 4000000000)
	if err != nil {
		t.Fatal(err)
	}
	var ev nostr.Event
	_ = ev.UnmarshalJSON(raw)
	got, ok := validEngram(&ev, ev.PubKey, owner, key)
	if !ok || got.Slug != "mem/example" || got.Plain != h.Plain || got.CreatedAt != 4000000001 {
		t.Fatalf("re-signed: %+v", got)
	}
}

// signed builds an engram as the spec's test agent would publish it, so
// each test fails for its own reason and not on a broken signature.
func signed(t *testing.T, tags nostr.Tags, plain string) *nostr.Event {
	t.Helper()
	secA := "0000000000000000000000000000000000000000000000000000000000000001"
	owner := "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	key, _ := nip44.GenerateConversationKey(owner, secA)
	content, _ := nip44.Encrypt(plain, key)
	ev := nostr.Event{Kind: 30174, CreatedAt: 1700000100, Tags: tags, Content: content}
	if err := ev.Sign(secA); err != nil {
		t.Fatal(err)
	}
	return &ev
}

func TestValidEngramRulesEachOnTheirOwn(t *testing.T) {
	secA := "0000000000000000000000000000000000000000000000000000000000000001"
	owner := "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	agent, _ := nostr.GetPublicKey(secA)
	key, _ := nip44.GenerateConversationKey(owner, secA)
	body := `{"slug":"mem/example","value":"v"}`
	d := fleet.DTag(key, "mem/example")

	if _, ok := validEngram(signed(t, nostr.Tags{{"d", d}, {"p", owner}}, body), agent, owner, key); !ok {
		t.Fatal("a well-formed signed engram is valid")
	}
	for name, ev := range map[string]*nostr.Event{
		"d tag of another slug": signed(t, nostr.Tags{{"d", fleet.DTag(key, "mem/other")}, {"p", owner}}, body),
		"two d tags":            signed(t, nostr.Tags{{"d", d}, {"d", d}, {"p", owner}}, body),
		"two p tags":            signed(t, nostr.Tags{{"d", d}, {"p", owner}, {"p", owner}}, body),
		"no p tag":              signed(t, nostr.Tags{{"d", d}}, body),
		"duplicate body key":    signed(t, nostr.Tags{{"d", d}, {"p", owner}}, `{"slug":"mem/example","value":"v","value":"w"}`),
	} {
		if _, ok := validEngram(ev, agent, owner, key); ok {
			t.Fatalf("%s must be invalid", name)
		}
	}
}

func TestPaginateReadsPastTheRelaysPageCap(t *testing.T) {
	// A relay that silently returns at most cap events, newest first,
	// honouring until (inclusive), as Buzz does with its 1000-event page.
	var all []*nostr.Event
	for i := 0; i < 23; i++ {
		all = append(all, &nostr.Event{ID: fmt.Sprintf("e%02d", i), CreatedAt: nostr.Timestamp(1000 + i/2)})
	}
	relay := func(cap int) func(nostr.Filter) ([]*nostr.Event, error) {
		return func(f nostr.Filter) ([]*nostr.Event, error) {
			var out []*nostr.Event
			for i := len(all) - 1; i >= 0; i-- {
				if f.Until != nil && all[i].CreatedAt > *f.Until {
					continue
				}
				out = append(out, all[i])
				if len(out) == cap || len(out) == f.Limit {
					break
				}
			}
			return out, nil
		}
	}
	got, err := paginate(relay(5), nostr.Filter{}, 5)
	if err != nil || len(got) != 23 {
		t.Fatalf("got %d events (%v), want all 23 past a cap of 5", len(got), err)
	}
	// A second holding more than a page cannot be paged through.
	same := make([]*nostr.Event, 7)
	for i := range same {
		same[i] = &nostr.Event{ID: fmt.Sprintf("s%d", i), CreatedAt: 1}
	}
	all = same
	if _, err := paginate(relay(5), nostr.Filter{}, 5); err == nil {
		t.Fatal("an unfinishable listing must fail, not come back short")
	}
}

func TestPageNeverExceedsTheRelaysCap(t *testing.T) {
	if capPage(&nip11.RelayLimitationDocument{MaxLimit: 100}, nil) != 100 {
		t.Fatal("a relay capped below our page: ask for its cap, so a short page still means done")
	}
	if capPage(&nip11.RelayLimitationDocument{MaxLimit: 1000}, nil) != engramPage || capPage(nil, nil) != engramPage || capPage(nil, fmt.Errorf("x")) != engramPage {
		t.Fatal("otherwise our own page")
	}
}
