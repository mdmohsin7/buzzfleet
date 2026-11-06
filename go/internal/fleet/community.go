package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ── one agent, several communities ────────────────────────────────────────────
//
// buzz-acp connects to exactly one relay, so an agent in several communities
// is several harnesses: one per community, all on the same identity, workdir
// and model login. The first community is the one the agent was created in;
// its settings stay at the top level of agent.json, untouched, so a binary
// that predates communities still runs it. Every other community is an entry
// under "communities", and gets its own unit, buzz-agent@<agent>--<community>.
// See docs/design/multi-community.md.

// MainCommunity names the community an agent was created in.
const MainCommunity = "main"

// instanceSep joins agent and community in a unit instance name. Agent names
// may contain single dashes, so the separator is a double one, which agent
// names may not contain.
const instanceSep = "--"

// AccessFull is the only access mode built so far: the harness sees everything
// the agent sees. Restricted access is designed but not built.
const AccessFull = "full"

// CommunityConfig is one extra community in agent.json.
type CommunityConfig struct {
	Relay     string `json:"relay"`
	Owner     string `json:"owner,omitempty"`
	RespondTo string `json:"respondTo,omitempty"`
	Access    string `json:"access"`
	JoinedAt  int64  `json:"joinedAt,omitempty"`
}

var (
	communityNameRe = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	agentNameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// ValidCommunityName: short, lowercase, no dashes (a dash separates agent from
// community in unit names), and never "main", which is implicit.
func ValidCommunityName(name string) error {
	if !communityNameRe.MatchString(name) {
		return fmt.Errorf("community name %q must be 1-16 lowercase letters or digits", name)
	}
	if name == MainCommunity {
		return fmt.Errorf("%q is the community the agent was created in; pick another name", name)
	}
	return nil
}

// ValidAgentName rejects names that would be ambiguous in a unit instance.
func ValidAgentName(name string) error {
	if !agentNameRe.MatchString(name) || strings.Contains(name, instanceSep) || strings.HasSuffix(name, "-") {
		return fmt.Errorf("agent name %q must be lowercase letters, digits, _ and single dashes, not ending in a dash: "+
			"a double dash separates agent from community in unit names", name)
	}
	return nil
}

// Harness is one running presence of an agent: the agent in one community.
type Harness struct {
	Agent     string
	Community string // MainCommunity for the one the agent was created in
}

// Instance is the systemd instance name: "agent" for the main community, so
// existing units keep their names, and "agent--community" otherwise.
func (h Harness) Instance() string {
	if h.Community == "" || h.Community == MainCommunity {
		return h.Agent
	}
	return h.Agent + instanceSep + h.Community
}

// Label is how the harness is shown and addressed. An agent in one community
// is shown as before, by its name alone; once it has more, each harness is
// agent@community.
func (h Harness) Label(multi bool) string {
	if !multi {
		return h.Agent
	}
	c := h.Community
	if c == "" {
		c = MainCommunity
	}
	return h.Agent + "@" + c
}

// ParseInstance reads a unit instance name back into a harness.
func ParseInstance(instance string) Harness {
	if a, c, ok := strings.Cut(instance, instanceSep); ok {
		return Harness{Agent: a, Community: c}
	}
	return Harness{Agent: instance, Community: MainCommunity}
}

// ParseTarget reads what a command was pointed at: "agent" means every
// harness of the agent, "agent@community" exactly one.
func ParseTarget(s string) (agent, community string, all bool) {
	if a, c, ok := strings.Cut(s, "@"); ok {
		if c == "" {
			c = MainCommunity
		}
		return a, c, false
	}
	return s, "", true
}

// CanonicalRelay is the form relays are compared in: scheme folded to ws/wss,
// host lowercased, default port and trailing slash dropped. The same relay
// written two ways is still one community, and two harnesses on one key and
// one relay answer every mention twice.
func CanonicalRelay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https":
		scheme = "wss"
	case "http":
		scheme = "ws"
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && !(scheme == "wss" && p == "443") && !(scheme == "ws" && p == "80") {
		host += ":" + p
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return scheme + "://" + host + path
}

// CheckCommunities validates an agent's extra communities against its main
// relay: valid names, a relay each, a known access mode, and no relay used
// twice.
func CheckCommunities(mainRelay string, cs map[string]CommunityConfig) error {
	seen := map[string]string{}
	if mainRelay != "" {
		seen[CanonicalRelay(mainRelay)] = MainCommunity
	}
	names := make([]string, 0, len(cs))
	for n := range cs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := cs[n]
		if err := ValidCommunityName(n); err != nil {
			return err
		}
		if c.Relay == "" {
			return fmt.Errorf("community %q has no relay", n)
		}
		if c.Access != AccessFull {
			return fmt.Errorf("community %q: access %q is not supported (only %q is built)", n, c.Access, AccessFull)
		}
		k := CanonicalRelay(c.Relay)
		if other, dup := seen[k]; dup {
			return fmt.Errorf("communities %q and %q are on the same relay; one identity on one relay twice answers every mention twice", other, n)
		}
		seen[k] = n
	}
	return nil
}

// ── editing agent.json without disturbing it ──────────────────────────────────
//
// agent.json is hand-edited and read by older binaries. Re-marshalling it
// through a Go map would reorder its keys and reformat it; worse, it would
// silently drop any field this version does not model. So the communities key
// is spliced into the original bytes instead, and everything else is left
// exactly as it was.

// topLevelSpan finds the byte range of a top-level key's member, from the
// key's opening quote to the end of its value.
func topLevelSpan(doc []byte, key string) (start, valStart, end int, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	t, err := dec.Token()
	if err != nil {
		return 0, 0, 0, false, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return 0, 0, 0, false, fmt.Errorf("agent.json is not a JSON object")
	}
	for dec.More() {
		before := int(dec.InputOffset())
		kt, err := dec.Token()
		if err != nil {
			return 0, 0, 0, false, err
		}
		k, _ := kt.(string)
		afterKey := int(dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, 0, false, err
		}
		if k == key {
			// InputOffset sits after the previous token; skip separators to
			// land on the key's quote.
			s := before
			for s < len(doc) && doc[s] != '"' {
				s++
			}
			v := afterKey
			for v < len(doc) && (doc[v] == ':' || isSpace(doc[v])) {
				v++
			}
			return s, v, int(dec.InputOffset()), true, nil
		}
	}
	if _, err := dec.Token(); err != nil && err != io.EOF {
		return 0, 0, 0, false, err
	}
	return 0, 0, 0, false, nil
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// SetTopLevelKey replaces a top-level key's value, or adds the key at the end
// of the object, leaving every other byte of the document as it was.
func SetTopLevelKey(doc []byte, key string, value []byte) ([]byte, error) {
	_, valStart, end, found, err := topLevelSpan(doc, key)
	if err != nil {
		return nil, err
	}
	if found {
		return concat(doc[:valStart], value, doc[end:]), nil
	}
	close := bytes.LastIndexByte(doc, '}')
	if close < 0 {
		return nil, fmt.Errorf("agent.json is not a JSON object")
	}
	body := bytes.TrimRight(doc[:close], " \t\r\n")
	kq, _ := json.Marshal(key)
	sep := ",\n  "
	if bytes.HasSuffix(body, []byte("{")) {
		sep = "\n  "
	}
	return concat(body, []byte(sep), kq, []byte(": "), value, []byte("\n"), doc[close:]), nil
}

// DeleteTopLevelKey removes a top-level key and its separating comma.
func DeleteTopLevelKey(doc []byte, key string) ([]byte, error) {
	start, _, end, found, err := topLevelSpan(doc, key)
	if err != nil || !found {
		return doc, err
	}
	// Take the comma before the member if there is one; otherwise the one
	// after it, so the object stays valid either way.
	i := start - 1
	for i >= 0 && isSpace(doc[i]) {
		i--
	}
	if i >= 0 && doc[i] == ',' {
		return concat(doc[:i], doc[end:]), nil
	}
	j := end
	for j < len(doc) && isSpace(doc[j]) {
		j++
	}
	if j < len(doc) && doc[j] == ',' {
		j++
		for j < len(doc) && isSpace(doc[j]) {
			j++
		}
		return concat(doc[:start], doc[j:]), nil
	}
	return concat(doc[:start], doc[end:]), nil
}

func concat(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, p := range parts {
		b.Write(p)
	}
	return b.Bytes()
}
