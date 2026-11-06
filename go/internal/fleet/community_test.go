package fleet

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHarnessNaming(t *testing.T) {
	main := Harness{Agent: "agent-a", Community: MainCommunity}
	b := Harness{Agent: "agent-a", Community: "b"}
	if main.Instance() != "agent-a" || (Harness{Agent: "agent-a"}).Instance() != "agent-a" {
		t.Fatal("the main harness keeps the unit name it always had")
	}
	if b.Instance() != "agent-a--b" {
		t.Fatalf("got %s", b.Instance())
	}
	for _, h := range []Harness{main, b} {
		if got := ParseInstance(h.Instance()); got != h {
			t.Fatalf("round trip %+v -> %+v", h, got)
		}
	}
	if main.Label(false) != "agent-a" || main.Label(true) != "agent-a@main" || b.Label(true) != "agent-a@b" {
		t.Fatal("labels")
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in       string
		agent, c string
		all      bool
	}{
		{"agent-a", "agent-a", "", true},
		{"agent-a@b", "agent-a", "b", false},
		{"agent-a@main", "agent-a", MainCommunity, false},
		{"agent-a@", "agent-a", MainCommunity, false},
	}
	for _, c := range cases {
		a, comm, all := ParseTarget(c.in)
		if a != c.agent || comm != c.c || all != c.all {
			t.Fatalf("%s: %s %s %v", c.in, a, comm, all)
		}
	}
}

func TestNames(t *testing.T) {
	for _, ok := range []string{"b", "ops2", "abcdefghijklmnop"} {
		if err := ValidCommunityName(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "main", "with-dash", "UPPER", "abcdefghijklmnopq", "a@b", "a.b"} {
		if ValidCommunityName(bad) == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
	for _, ok := range []string{"agent-a", "a", "agent2", "a_b"} {
		if err := ValidAgentName(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"agent--a", "-a", "a-", "Agent", "a.b", ""} {
		if ValidAgentName(bad) == nil {
			t.Fatalf("%q should be refused: it would be ambiguous in a unit name", bad)
		}
	}
}

func TestCanonicalRelay(t *testing.T) {
	same := []string{
		"wss://Relay-B.example", "wss://relay-b.example/", "https://relay-b.example",
		"wss://relay-b.example:443", " wss://relay-b.example ",
	}
	for _, s := range same {
		if got := CanonicalRelay(s); got != "wss://relay-b.example" {
			t.Fatalf("%q -> %q", s, got)
		}
	}
	if CanonicalRelay("ws://relay-b.example:8080/x/") != "ws://relay-b.example:8080/x" {
		t.Fatal("non-default port and path are kept")
	}
	if CanonicalRelay("wss://relay-b.example/x") == CanonicalRelay("wss://relay-b.example/y") {
		t.Fatal("paths distinguish relays")
	}
}

func TestCheckCommunities(t *testing.T) {
	ok := map[string]CommunityConfig{
		"b": {Relay: "wss://relay-b.example", Access: AccessFull},
		"c": {Relay: "wss://relay-c.example", Access: AccessFull},
	}
	if err := CheckCommunities("wss://relay-a.example", ok); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]CommunityConfig{
		"same relay as main": {"b": {Relay: "https://RELAY-A.example/", Access: AccessFull}},
		"same relay twice": {
			"b": {Relay: "wss://relay-b.example", Access: AccessFull},
			"c": {Relay: "wss://relay-b.example:443", Access: AccessFull},
		},
		"no relay":             {"b": {Access: AccessFull}},
		"restricted not built": {"b": {Relay: "wss://relay-b.example", Access: "restricted"}},
		"no access":            {"b": {Relay: "wss://relay-b.example"}},
		"bad name":             {"main": {Relay: "wss://relay-b.example", Access: AccessFull}},
	}
	for name, cs := range cases {
		if CheckCommunities("wss://relay-a.example", cs) == nil {
			t.Fatalf("%s: should be refused", name)
		}
	}
}

const legacyAgentJSON = `{
  "name": "agent-a",
  "workdir": "/srv/agent-a",
    "relay": "wss://relay-a.example",
  "owner": "aaaa",
  "x-unknown-field": {"keep": [1, 2, 3]},
  "respondTo": "owner-only"
}
`

func TestSetTopLevelKeyPreservesEverythingElse(t *testing.T) {
	v := []byte(`{"b": {"relay": "wss://relay-b.example", "access": "full"}}`)
	out, err := SetTopLevelKey([]byte(legacyAgentJSON), "communities", v)
	if err != nil {
		t.Fatal(err)
	}
	// Every original byte up to the closing brace survives untouched: odd
	// indentation, key order and fields this version does not know about.
	orig := strings.TrimRight(strings.TrimSuffix(strings.TrimSpace(legacyAgentJSON), "}"), " \n")
	if !strings.HasPrefix(string(out), orig) {
		t.Fatalf("original bytes changed:\n%s", out)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if string(doc["communities"]) != string(v) || string(doc["x-unknown-field"]) != `{"keep": [1, 2, 3]}` {
		t.Fatalf("got %s", out)
	}

	// Replacing it again touches only its value.
	v2 := []byte(`{}`)
	out2, err := SetTopLevelKey(out, "communities", v2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out2), orig) || !strings.Contains(string(out2), `"communities": {}`) {
		t.Fatalf("got %s", out2)
	}

	// And removing it gives back the original document, byte for byte.
	back, err := DeleteTopLevelKey(out, "communities")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(back)) != strings.TrimSpace(legacyAgentJSON) {
		t.Fatalf("round trip changed the file:\n%q\n%q", back, legacyAgentJSON)
	}
}

func TestTopLevelKeyEdgeCases(t *testing.T) {
	// A nested key of the same name is not the top-level one.
	doc := []byte(`{"x": {"communities": 1}, "y": 2}`)
	out, _ := SetTopLevelKey(doc, "communities", []byte(`{}`))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil || string(m["x"]) != `{"communities": 1}` || string(m["communities"]) != `{}` {
		t.Fatalf("got %s (%v)", out, err)
	}
	// Removing a first, middle or only member keeps valid JSON.
	for _, in := range []string{`{"communities": {}, "a": 1}`, `{"a": 1, "communities": {}, "b": 2}`, `{"communities": {}}`} {
		out, err := DeleteTopLevelKey([]byte(in), "communities")
		if err != nil || !json.Valid(out) || strings.Contains(string(out), "communities") {
			t.Fatalf("%s -> %s (%v)", in, out, err)
		}
	}
	if out, _ := SetTopLevelKey([]byte(`{}`), "communities", []byte(`{}`)); !json.Valid(out) {
		t.Fatalf("empty object: %s", out)
	}
	if _, err := SetTopLevelKey([]byte(`[1]`), "communities", []byte(`{}`)); err == nil {
		t.Fatal("not an object")
	}
}
