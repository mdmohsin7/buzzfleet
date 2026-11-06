package fleet

import (
	"strings"
	"testing"
)

func TestCheckPeekAllowsOnlyReads(t *testing.T) {
	ok := [][]string{
		{"messages", "search", "--query", "launch date"},
		{"messages", "thread", "--event", "abc"},
		{"channels", "list"},
		{"--format", "compact", "messages", "get", "--channel", "c"},
		{"mem", "get", "mem/notes"},
		{"users", "presence", "--pubkeys", "x"},
	}
	for _, a := range ok {
		if err := CheckPeek(a); err != nil {
			t.Fatalf("%v: %v", a, err)
		}
	}
	bad := [][]string{
		{"messages", "send", "--channel", "c", "--content", "hi"},
		{"messages", "delete", "--event", "e"},
		{"reactions", "add"},
		{"channels", "join", "--channel", "c"},
		{"mem", "set", "mem/x", "v"},
		{"mem", "rm", "mem/x"},
		{"users", "set-profile", "--name", "x"},
		{"--relay", "wss://elsewhere.example", "messages", "get"},
		{"--relay=wss://elsewhere.example", "messages", "get"},
		{"messages", "get", "--private-key", "k"},
		{"--auth-tag", "x", "messages", "get"},
		{"messages"},
		{},
	}
	for _, a := range bad {
		if CheckPeek(a) == nil {
			t.Fatalf("%v must be refused", a)
		}
	}
}

func TestCommunitiesPrompt(t *testing.T) {
	s := CommunitiesPrompt("b", []string{"main", "b", "c"}, "/x/buzzfleet peek", "/f/agents/agent-a/shared-notes.md")
	for _, want := range []string{
		"main, b, c", "answering in **b** now", "reply only here",
		"main or c", "/x/buzzfleet peek <community> messages search", "Never post",
		"/f/agents/agent-a/shared-notes.md", "Never write one community's private details",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "wss://") || strings.Contains(s, "://") {
		t.Fatal("communities are named by local name only, never by relay address")
	}
}
