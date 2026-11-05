package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mdmohsin7/buzzfleet/internal/fleet"
)

// How much of each transcript to read, from the end. One turn's records are a
// few KB; a tool result can be far bigger, so leave room for a few of those.
const transcriptTail = 512 << 10

// Transcripts older than this say nothing about the agent's current state.
const transcriptMaxAge = 7 * 24 * time.Hour

// How many of an agent's most recent sessions to read. Harnesses can keep one
// session per thread, so the newest file is not always the latest turn.
const transcriptFiles = 3

// AgentEnv reads the running harness's environment, which is where per-agent
// config dirs (CLAUDE_CONFIG_DIR, CODEX_HOME) are set.
func (h *Host) AgentEnv(name string) map[string]string {
	pid := h.MainPID(name)
	if pid == "" {
		return nil
	}
	b, err := os.ReadFile("/proc/" + pid + "/environ")
	if err != nil {
		return nil
	}
	env := map[string]string{}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if k, v, ok := strings.Cut(string(kv), "="); ok {
			env[k] = v
		}
	}
	return env
}

// InferenceSignals gathers recent turn outcomes for an agent from whichever
// coding agents have transcripts for its workdir.
func (h *Host) InferenceSignals(name string) []fleet.Signal {
	c, err := h.AgentConfig(name)
	if err != nil {
		return nil
	}
	env := h.AgentEnv(name)
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	claudeDir := env["CLAUDE_CONFIG_DIR"]
	if claudeDir == "" {
		claudeDir = filepath.Join(home, ".claude")
	}
	codexHome := env["CODEX_HOME"]
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	var workdirs []string
	for _, n := range h.Agents() {
		if oc, err := h.AgentConfig(n); err == nil {
			workdirs = append(workdirs, oc.Workdir)
		}
	}
	return signalsFor(claudeDir, codexHome, c.Workdir, workdirs, time.Now())
}

func signalsFor(claudeDir, codexHome, workdir string, workdirs []string, now time.Time) []fleet.Signal {
	sigs := readSignals(claudeTranscripts(claudeDir, workdir, workdirs, now), fleet.ParseClaudeLine)
	return append(sigs, readSignals(codexTranscripts(codexHome, workdir, workdirs, now), fleet.ParseCodexLine)...)
}

type fileAge struct {
	path string
	mod  time.Time
}

func newest(files []fileAge, n int) []string {
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	var out []string
	for i := 0; i < len(files) && i < n; i++ {
		out = append(out, files[i].path)
	}
	return out
}

// claudeTranscripts finds the agent's sessions. Claude Code keeps one
// directory per working directory, so an agent that cd's into a repo below its
// workdir writes to a sibling directory with a longer name. Only directories
// this agent owns count, not ones belonging to an agent whose workdir merely
// shares a prefix.
func claudeTranscripts(configDir, workdir string, workdirs []string, now time.Time) []string {
	root := filepath.Join(configDir, "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var files []fileAge
	for _, e := range entries {
		if !e.IsDir() || fleet.ClaudeProjectOwner(e.Name(), workdirs) != workdir {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(root, e.Name(), "*.jsonl"))
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && now.Sub(fi.ModTime()) < transcriptMaxAge {
				files = append(files, fileAge{m, fi.ModTime()})
			}
		}
	}
	return newest(files, transcriptFiles)
}

// codexTranscripts finds the agent's rollouts. Codex files them by date, not
// by directory, so each recent one is matched on the cwd in its first record.
func codexTranscripts(codexHome, workdir string, workdirs []string, now time.Time) []string {
	var files []fileAge
	for d := 0; d <= 7; d++ {
		day := now.AddDate(0, 0, -d)
		matches, _ := filepath.Glob(filepath.Join(codexHome, "sessions", day.Format("2006/01/02"), "rollout-*.jsonl"))
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err != nil || now.Sub(fi.ModTime()) >= transcriptMaxAge {
				continue
			}
			if fleet.DirOwner(codexSessionCwd(m), workdirs) == filepath.Clean(workdir) {
				files = append(files, fileAge{m, fi.ModTime()})
			}
		}
	}
	return newest(files, transcriptFiles)
}

func codexSessionCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 64<<10).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.Cwd
}

// readSignals parses the tail of each file.
func readSignals(paths []string, parse func([]byte) (fleet.Signal, bool)) []fleet.Signal {
	var out []fleet.Signal
	for _, p := range paths {
		for _, line := range tailLines(p, transcriptTail) {
			if s, ok := parse(line); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// tailLines returns the complete lines in the last n bytes of a file.
func tailLines(path string, n int64) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := fi.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	lines := bytes.Split(buf, []byte{'\n'})
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // starts mid-line
	}
	return lines
}
