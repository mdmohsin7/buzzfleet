package fleet

import (
	"fmt"
	"sort"
	"strings"
)

// ── reading another community ─────────────────────────────────────────────────
//
// An agent in several communities answers each from its own harness, but what
// it knows should not stop at a community's edge: asked in one about something
// decided in another, it should be able to look. `buzzfleet peek` runs the buzz
// CLI against another of the agent's communities, as the agent, with only read
// subcommands allowed. Posting stays in the community that asked.
//
// This is advisory. The harness holds the agent's key and could point the buzz
// CLI at the other relay itself; peek is the tool it is told to use, and the
// prompt tells it never to post across.

// peekAllowed is every buzz subcommand peek will run, by group.
var peekAllowed = map[string][]string{
	"messages": {"get", "thread", "search"},
	"channels": {"list", "get", "search", "members"},
	"feed":     {"get"},
	"users":    {"get", "presence"},
	"mem":      {"ls", "get", "hash"},
}

// Global flags that would change which relay or which identity the call uses.
var peekForbiddenFlags = []string{"--relay", "--private-key", "--auth-tag"}

// CheckPeek refuses anything but a read in the allowed list.
func CheckPeek(args []string) error {
	// Global flags are accepted anywhere on the line, so check all of it.
	for _, a := range args {
		for _, f := range peekForbiddenFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("peek sets the relay and identity itself; %s is not allowed", f)
			}
		}
	}
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--format" {
			i++ // takes a value
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		words = append(words, a)
		if len(words) == 2 {
			break
		}
	}
	if len(words) < 2 {
		return fmt.Errorf("usage: peek <community> <group> <command> [flags]; allowed: %s", PeekUsage())
	}
	for _, sub := range peekAllowed[words[0]] {
		if sub == words[1] {
			return nil
		}
	}
	return fmt.Errorf("peek only reads; %q %q is not allowed. Allowed: %s", words[0], words[1], PeekUsage())
}

// PeekUsage lists what peek allows, for errors and the prompt.
func PeekUsage() string {
	groups := make([]string, 0, len(peekAllowed))
	for g := range peekAllowed {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	var parts []string
	for _, g := range groups {
		parts = append(parts, g+" "+strings.Join(peekAllowed[g], "|"))
	}
	return strings.Join(parts, ", ")
}

// CommunitiesPrompt is the section added to a multi-community agent's system
// prompt, telling it where it is, what it shares, and how to look elsewhere.
// Communities are named by their local names only: a relay address says
// nothing useful to the agent and would leak into whatever it writes.
func CommunitiesPrompt(current string, all []string, peekCmd, notesPath string) string {
	var others []string
	for _, c := range all {
		if c != current {
			others = append(others, c)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Your communities\n\n")
	fmt.Fprintf(&b, "You are one agent in several Buzz communities, with the same identity, the same workdir and the same memory in each: %s. ", strings.Join(all, ", "))
	fmt.Fprintf(&b, "You are answering in **%s** now, and you reply only here.\n\n", current)
	fmt.Fprintf(&b, "- What you know from your workdir and your own memory comes with you into every community. People here may not know the others exist. Bring something from another community only when it clearly belongs here, and never anything that was said in confidence there.\n")
	if len(others) > 0 {
		fmt.Fprintf(&b, "- To look something up in %s, read it with `%s <community> messages search --query \"...\"`. Also allowed: %s. Never post, react, join or edit anything in another community.\n",
			strings.Join(others, " or "), peekCmd, PeekUsage())
	}
	if notesPath != "" {
		fmt.Fprintf(&b, "- Shared notes for all your communities are in `%s`. Read it when a session starts. Add durable facts that would help you in every community. Never write one community's private details there.\n", notesPath)
	}
	return b.String()
}
