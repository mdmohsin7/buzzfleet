# buzzfleet

Run [Buzz](https://github.com/block/buzz) agents on your own server, and manage them from your phone.

```
@fleet status

AGENT       STATE     BOOT  PID       UP    WORKDIR
charles     active    yes   4067374   6h    /srv/omi
scout       active    yes   4066466   6h    /srv/scratch

charles     ok        present on relay
scout       ok        present on relay
```

That works from the desktop app, the mobile app and the web client, because it
is a chat message.

## Why

Buzz agents run happily on a server, but there is nothing to operate them with
once they are there. The desktop Agents view lists only agents that app hosts
itself, and `!shutdown` / `!cancel` / `!rotate` run *inside* each harness — so
they only ever reach an agent that is still working.

The failure that matters is the one you cannot see: a harness can be running,
its service green, and answering nothing. Presence quietly stops reaching the
relay while the process stays alive. buzzfleet watches from outside, notices
within about 90 seconds, and restarts it.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/mdmohsin7/buzzfleet/main/install.sh | sh
```

One static binary, no runtime. It writes its own systemd units and tells you
what the host is missing.

From source:

```bash
git clone https://github.com/mdmohsin7/buzzfleet.git && cd buzzfleet
make build && ./bin/buzzfleet install
```

## Setup

Mint an invite in the Buzz app — right-click your community, *Invite to
community* — then:

```bash
buzzfleet init --invite v2....
```

That gives the supervisor its own identity, joins it to your community, picks a
control channel, and works out who the owner is from the member list. It
introduces itself in the channel with its command list.

Add an agent:

```bash
buzzfleet add scout --workdir /srv/scratch --invite v2.... \
  --display-name "Scout" --prompt "You research things and report back briefly."
buzzfleet channels scout
buzzfleet join scout <channel-uuid>
```

## Commands

From chat, addressed to the supervisor:

```
status              agents on this host: state, uptime, health
health              is each agent actually reachable on the relay
restart <agent>
start | stop <agent>
logs <agent> [n]
prompt <agent> [text]   show or set its personality
doctor              duplicates, permissions, membership
```

Only the owner is obeyed. Short replies land inline; long ones are threaded.

Each host answers to its own name and to a shared `@fleet`, so one message
reaches every machine and each reply is labelled with its hostname.

An agent's personality is its system prompt, set at creation with `--prompt` or
`--prompt-file` and changed later from either surface:

```bash
buzzfleet prompt scout --edit          # or --set "..." / --file prompt.md
```

It is a plain file at `agents/<name>/system-prompt.md`; the agent restarts to
pick it up.

From the shell, the same operations plus `add`, `rename`, `reap`, `join`,
`channels`, `install` and `init`. Run `buzzfleet` for the full list.

## Health

| state | meaning |
|---|---|
| `ok` | running, present on the relay, agent process alive |
| `wedged` | running, but the relay cannot see it |
| `stalled` | present on the relay, but its agent process is gone |
| `down` | not running |
| `unknown` | the relay was unreachable — never blamed on the agent |

An agent can also be up, present and answering while every turn fails: its
model login expired, or the account ran out of usage. The supervisor reads the
coding agent's own transcripts (Claude Code under `$CLAUDE_CONFIG_DIR/projects`,
Codex under `$CODEX_HOME/sessions`, for the agent's workdir and repos below it)
and the latest turn decides:

| state | meaning |
|---|---|
| `auth` | the model provider rejected the agent's login (401 / authentication_failed). Claude Code only: Codex leaves no record of an auth failure |
| `limited` | the account hit a usage limit (429 / rate_limit), shown with its reset time when the provider gave one. For Codex, read from the usage snapshot it records with each token count: a window at 100% (without credits) or a reached-limit marker |

Neither is ever restarted, because a restart fixes neither. The supervisor
says so once in the channel, naming every agent that went down for the same
reason in one message, since one account limit takes them all at once:

```
host-1 — agent-a, agent-b, agent-c are out of model usage (usage limit until Jan 12 22:00 UTC).
Every turn is failing, so I am not restarting them: a restart does not fix this.
```

The state clears on the agent's next good turn. A limit with a known reset is
also assumed lifted once the reset passes; the supervisor says the agent
"should work again" rather than that it recovered, since nothing has run yet.

Transcripts are matched to agents by workdir. Claude Code's directory names
turn both `/` and `-` into `-`, so `/srv/agent/b` and `/srv/agent-b` look the
same; give agents workdirs that do not nest inside each other's names.
Only the coding agent's own error records count: a `401` printed by a command
the agent ran is not its credentials failing.

`wedged` and `stalled` are healed automatically after two consecutive
confirmations, and the supervisor says what it did in the channel. It stops
after three restarts an hour and tells you once, because an agent a restart
cannot fix should not be restarted forever.

## Layout

```
~/.buzzfleet/
  fleetd.json           supervisor identity and config
  agents/<name>/
    identity.env        0600 — the agent's key
    agent.json          workdir, relay, owner
    system-prompt.md    its persona
```

One systemd user unit per agent, from a single template.

## Requirements

- Linux with systemd to supervise agents, and lingering enabled so they survive
  a reboot: `sudo loginctl enable-linger $USER`
- The `buzz` CLI on `PATH`, or `BUZZ_BIN` pointing at it
- An agent runtime such as `claude-agent-acp`, `codex-acp` or `goose`

## Licence

MIT.
