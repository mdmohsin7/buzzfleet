package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

func write(t *testing.T, path string, mod time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

const (
	okTurn   = `{"type":"assistant","timestamp":"%s","message":{"content":[{"type":"text","text":"done"}]}}`
	authTurn = `{"type":"assistant","timestamp":"%s","isApiErrorMessage":true,"error":"authentication_failed","message":{"content":[{"type":"text","text":"login expired"}]}}`
)

func turn(tmpl string, at time.Time) string {
	return strings.Replace(tmpl, "%s", at.UTC().Format(time.RFC3339Nano), 1)
}

func TestClaudeSignalsComeFromTheAgentsOwnProjects(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	wd := []string{"/srv/agent", "/srv/agent-b"}

	// The agent's own workdir, and a repo it cd'd into: both count.
	write(t, filepath.Join(claude, "projects", "-srv-agent", "s1.jsonl"), now.Add(-time.Hour), turn(okTurn, now.Add(-time.Hour)))
	write(t, filepath.Join(claude, "projects", "-srv-agent-repos-api", "s2.jsonl"), now, turn(authTurn, now.Add(-time.Minute)))
	// A neighbour whose workdir shares the prefix: its failures are not ours.
	write(t, filepath.Join(claude, "projects", "-srv-agent-b", "s3.jsonl"), now, turn(authTurn, now))
	// Stale: says nothing about now.
	write(t, filepath.Join(claude, "projects", "-srv-agent", "old.jsonl"), now.AddDate(0, 0, -30), turn(authTurn, now.AddDate(0, 0, -30)))

	got := signalsFor(claude, filepath.Join(root, "codex"), "/srv/agent", wd, now)
	if len(got) != 2 {
		t.Fatalf("want the 2 recent signals from the agent's own projects, got %d", len(got))
	}
	if st, _ := fleet.InferenceState(fleet.Latest(got), now); st != fleet.StateAuth {
		t.Fatalf("latest turn failed auth, got %s", st)
	}
	if st, _ := fleet.InferenceState(fleet.Latest(signalsFor(claude, "", "/srv/agent-b", wd, now)), now); st != fleet.StateAuth {
		t.Fatalf("neighbour: %s", st)
	}
}

func TestCodexSignalsMatchedOnSessionCwd(t *testing.T) {
	now := time.Now()
	codex := filepath.Join(t.TempDir(), "codex")
	day := filepath.Join(codex, "sessions", now.Format("2006/01/02"))
	ts := now.UTC().Format(time.RFC3339Nano)
	meta := func(cwd string) string {
		return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"cwd":"` + cwd + `"}}`
	}
	limit := `{"timestamp":"` + ts + `","type":"event_msg","payload":{"type":"error","message":"usage limit","codex_error_info":"usage_limit_exceeded"}}`
	tool := `{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"function_call_output","output":"HTTP 401 Unauthorized"}}`

	write(t, filepath.Join(day, "rollout-a.jsonl"), now, meta("/srv/agent-b/repo"), limit)
	write(t, filepath.Join(day, "rollout-b.jsonl"), now, meta("/srv/agent"), tool)

	wd := []string{"/srv/agent", "/srv/agent-b"}
	if got := signalsFor("", codex, "/srv/agent", wd, now); len(got) != 0 {
		t.Fatalf("tool output and a neighbour's session are not this agent's signals: %+v", got)
	}
	got := signalsFor("", codex, "/srv/agent-b", wd, now)
	if st, _ := fleet.InferenceState(fleet.Latest(got), now); st != fleet.StateLimited {
		t.Fatalf("got %s from %+v", st, got)
	}
}

func TestTailSkipsThePartialFirstLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, time.Now(), strings.Repeat("x", 100), "second", "third")
	lines := tailLines(p, 20)
	if len(lines) == 0 || string(lines[0]) != "second" {
		t.Fatalf("got %q", lines)
	}
}
