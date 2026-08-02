package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mdmohsin7/buzzfleet/internal/host"
)

// An agent's personality is its system prompt, and it is a plain file on disk:
// agents/<name>/system-prompt.md. Setting it needs no relay, no catalog and no
// desktop — which matters, because Buzz's own persona events are readable only
// by their author unless shared, so nothing outside the app can act on them.

func promptPath(h *host.Host, agent string) string {
	return filepath.Join(h.AgentsDir(), agent, "system-prompt.md")
}

func readPrompt(h *host.Host, agent string) string {
	b, err := os.ReadFile(promptPath(h, agent))
	if err != nil {
		return ""
	}
	return string(b)
}

// writePrompt replaces an agent's personality and restarts it, because the
// prompt is passed to the harness as a process argument and only takes effect
// on the next start.
func writePrompt(h *host.Host, agent, text string) error {
	if _, err := h.AgentConfig(agent); err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("refusing to set an empty personality for %s", agent)
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return os.WriteFile(promptPath(h, agent), []byte(text), 0o644)
}

func cmdPrompt(h *host.Host, args []string) {
	if len(args) == 0 {
		die("usage:\n" +
			"  buzzfleet prompt <agent>                 show it\n" +
			"  buzzfleet prompt <agent> --file <path>   set it from a file\n" +
			"  buzzfleet prompt <agent> --set \"...\"     set it inline\n" +
			"  buzzfleet prompt <agent> --edit          open it in $EDITOR")
	}
	agent := args[0]
	if _, err := h.AgentConfig(agent); err != nil {
		die("%v", err)
	}
	flags := parseFlags(args[1:])

	switch {
	case flags["file"] != "":
		b, err := os.ReadFile(flags["file"])
		if err != nil {
			die("cannot read %s: %v", flags["file"], err)
		}
		apply(h, agent, string(b))
	case flags["set"] != "":
		apply(h, agent, flags["set"])
	case contains(args[1:], "--edit"):
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		before := readPrompt(h, agent)
		if err := runEditor(editor, promptPath(h, agent)); err != nil {
			die("%v", err)
		}
		if readPrompt(h, agent) == before {
			fmt.Println("unchanged")
			return
		}
		restartFor(h, agent)
	default:
		p := readPrompt(h, agent)
		if p == "" {
			fmt.Printf("%s has no personality set\n", agent)
			return
		}
		fmt.Print(p)
	}
}

func apply(h *host.Host, agent, text string) {
	if err := writePrompt(h, agent, text); err != nil {
		die("%v", err)
	}
	restartFor(h, agent)
}

func restartFor(h *host.Host, agent string) {
	fmt.Printf("%s: personality updated\n", agent)
	if h.UnitState(agent) == "active" {
		cmdLifecycle(h, "restart", []string{agent})
	}
}

func runEditor(editor, path string) error {
	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
