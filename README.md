# buzzfleet

Run [Buzz](https://github.com/block/buzz) agents on your own server, and manage them from your phone.

```
@fleet status

AGENT       STATE     BOOT  PID       UP    WORKDIR
agent-a     active    yes   41207     6h    /srv/agent-a
agent-b     active    yes   41388     6h    /srv/agent-b

agent-a     ok        present on relay
agent-b     ok        present on relay
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
buzzfleet add agent-b --workdir /srv/agent-b --invite v2.... \
  --display-name "Agent B" --prompt "You research things and report back briefly."
buzzfleet channels agent-b
buzzfleet join agent-b <channel-uuid>
```

## Commands

From chat, addressed to the supervisor:

```
status              agents on this host: state, uptime, health
health              is each agent actually reachable on the relay
restart <agent>[@community]
start | stop <agent>[@community]
logs <agent> [n]
prompt <agent> [text]   show or set its personality
doctor              duplicates, permissions, membership, enforced limits
```

Only the owner is obeyed. Short replies land inline; long ones are threaded.

Each host answers to its own name and to a shared `@fleet`, so one message
reaches every machine and each reply is labelled with its hostname.

An agent's personality is its system prompt, set at creation with `--prompt` or
`--prompt-file` and changed later from either surface:

```bash
buzzfleet prompt agent-b --edit          # or --set "..." / --file prompt.md
```

It is a plain file at `agents/<name>/system-prompt.md`; the agent restarts to
pick it up.

From the shell, the same operations plus `add`, `rename`, `reap`, `join`,
`channels`, `limits`, `install` and `init`. Run `buzzfleet` for the full list.

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

## One agent, several communities

An agent can serve more than one community with the same identity, workdir and
model login, so what it has learned in one is there in the other:

```bash
buzzfleet community add agent-a b --invite v2.... --relay wss://<community b relay> --access full
buzzfleet channels agent-a@b
buzzfleet join agent-a@b <channel-uuid>
```

Each community is its own harness, because buzz-acp talks to one relay:
`buzz-agent@agent-a--b`, next to the original `buzz-agent@agent-a`, which is
left exactly as it was. The new one links the original's drop-ins (model,
environment), so the two cannot drift apart, and it copies the agent's
profile into the new community.

```
AGENT           STATE     BOOT  PID      UP     WORKDIR
agent-a@main    active    yes   41207    6h     /srv/agent-a
agent-a@b       active    yes   41388    2m     /srv/agent-a
```

Every command takes `agent-a` (all of its harnesses) or `agent-a@b` (one):
`restart`, `stop`, `start`, `logs`, `channels`, `join`, and the same from chat.
`community ls` lists them; `community rm agent-a b` stops that harness and
removes it, leaving `agent.json` byte-for-byte as it was before.

`--access full` means the community sees everything the agent sees: its whole
workdir, its coding agent's memory, and, through shared transcripts, its message
history from its other communities. A restricted mode is designed but not built;
see [the design](docs/design/multi-community.md). One identity on one relay
twice would answer every mention twice, so a second community on a relay the
agent already uses is refused, and `doctor` reports duplicates per identity and
relay.

### What the agent knows about its communities

An agent in more than one community is told so in its system prompt: which
communities it is in (by their local names, never by address), which one it is
answering in, and that it replies only there. Its personality stays one
`system-prompt.md`; a community can add to it:

```bash
buzzfleet prompt agent-a@b --set "In this community you are also its research assistant."
buzzfleet prompt agent-a@b            # show it
buzzfleet prompt agent-a@b --clear
```

That is `agents/agent-a/system-prompt.b.md`, and setting it restarts only that
community's harness. The same works from chat.

To look something up in another of its communities, the agent runs
`buzzfleet peek <community> messages search --query "..."`. Peek runs the buzz
CLI against that community as the agent, with reads only: `messages
get|thread|search`, `channels list|get|search|members`, `feed get`, `users
get|presence`, `mem ls|get|hash`. It refuses anything that posts, edits or
joins, and any flag that would change the relay or identity. This is a
convention the agent is told to keep, not a wall: it holds its own key and
could reach the other relay itself.

`agents/agent-a/shared-notes.md` is shared by every community's harness: the
agent reads it when a session starts and adds durable facts there, never one
community's private details.

### One turn at a time

The harnesses share one workdir, so they take turns: an agent works on one
community's request at a time, as it always has in one community. A request
that arrives while it is busy elsewhere waits, and after a minute the agent
says so where it was asked ("Busy with another task right now. This is
queued…"), without saying where. Turns are first come, first served; a busy
community cannot keep the agent from another. `health` shows it:

```
agent-a@main    ok       present on relay; in a turn since 14:02 UTC
agent-a@b       ok       present on relay; waiting on agent-a@main since 14:05 UTC
```

Waiting is shown, never acted on. The wait is bounded by the turn limit (2 h by
default). While a prompt waits, other messages for its session still reach the
coding agent; buzz-acp drives one coding-agent process at a time, so nothing
depends on that order today.

Claude Code can run a long command in the background, end its turn, and carry
on by itself when the command finishes, with no new prompt. For a harness
behind the gate, buzzfleet sets `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` so its
work stays inside the turn. As a backstop, a turn that still starts
background work keeps the agent's turn after its prompt ends, until the work
it does on waking has gone quiet (capped at 30 minutes); work the agent starts
on its own takes the turn if it is free, and is logged as an overlap if it is
not.

This is done by a small gate between buzz-acp and the coding agent, installed
under the coding agent's own name so buzz-acp treats it exactly as before. It
passes everything through except that a prompt waits for the agent's turn
lock. `BUZZ_FLEET_NO_GATE=1` in a drop-in turns it off; `doctor` warns when an
agent in several communities has a harness without it.

Downgrading to a release without communities: `community rm` each extra
community first. An older binary keeps running the original harness but cannot
see the others.

## Resource limits

A small host running several coding agents is one parallel test suite away
from a load of 50, and then sshd, the supervisor and every agent crawl
together. `buzzfleet limits` sizes a ceiling for all the agents together from
the host's cores and RAM, keeping half a core and an eighth of RAM (at least
1 GiB) for the host itself:

```
$ buzzfleet limits
host        2 cores, 12000M RAM, cgroup hybrid
agents      150% CPU and 10500M memory between them
containers  1 CPU and 3000M memory each, via the supervisor's Docker proxy

  sudo systemctl set-property user@1000.service CPUAccounting=yes MemoryAccounting=yes CPUQuota=150% MemoryLimit=10500M
```

`--apply` runs that command. Where the ceiling goes depends on the host:

- **cgroup v2 with cpu and memory delegated** to your user manager: on
  `buzzfleet.slice`, which the agent unit names and which holds exactly the
  agents. No root needed. Units installed before the template named its slice
  sit elsewhere: `buzzfleet install --force`, then restart the agents (`doctor`
  lists them).
- **cgroup v1, or v2 without delegation** (older distributions): on the
  system unit `user@UID.service`, with sudo. That holds every user unit, the
  supervisor included, but not SSH sessions, so a saturated fleet still cannot
  lock you out. Per-agent limits cannot work here at all: systemd accepts a
  `MemoryMax=` in an agent's drop-in and silently ignores it. On v1, systemd
  also leaves already-running processes outside a newly enabled cpu cgroup, so
  `--apply` moves them in itself, the user manager included, and fails
  loudly, listing the pids, if any are left outside.

Because systemd accepts limits it cannot apply, `doctor` reads the kernel's own
cgroup files for each agent and says which ceiling actually binds:

```
ok   agent-a: enforced CPU 150% (on user@1000.service), memory 10500M (on user@1000.service)
WARN agent-a: its unit sets MemoryMax=2048M but that is NOT enforced
```

Override any number in `fleetd.json`:

```json
"limits": { "cpuPercent": 120, "memoryMaxMB": 8000, "dockerCpus": 0.5, "dockerMemoryMB": 2048 }
```

### Containers

Containers are created by dockerd, outside the agent's cgroup, so they escape
every ceiling on it. The supervisor serves a Docker API proxy per agent, and
each agent's `DOCKER_HOST` points at it. On container create and update the
proxy fills in a CPU and memory cap where none was asked for and clamps one
that asks for more; a smaller request is kept as asked. Field names are
matched regardless of letter case, as dockerd matches them, and a body that
spells one field two ways is refused, as is one the proxy cannot parse. It also labels each
container `buzzfleet.agent=<name>` and `buzzfleet.created=<unix time>`. Compose,
SDKs and the CLI all go through it, interactive and streaming sessions included.

This is a guardrail, not a security boundary: an agent in the `docker` group can
still reach `/var/run/docker.sock` directly. `docker build` steps run inside
BuildKit rather than as containers, so they are not capped. Set
`"noDockerProxy": true` under `limits` to turn the proxy off. An agent picks up
the proxy when it next restarts, and `doctor` lists the ones that have not.

## Heartbeat

Everything the supervisor says goes through the relay. So when the relay or
the host is wedged, nothing says anything, and silence looks just like "all
fine". The optional heartbeat pushes host and fleet state straight to a
monitoring endpoint every minute. The receiver then alerts on bad values, and
on the heartbeat going quiet.

```json
"heartbeat": {
  "url": "https://<your influx endpoint>/api/v1/push/influx/write",
  "format": "influx",
  "basicUser": "<instance id>",
  "passwordFile": "/home/you/.buzzfleet/heartbeat.token"
}
```

- **`json`** (the default) is one document per beat, for any webhook or
  receiver: load, memory, swap, disk, uptime, and each agent's state, health,
  uptime and recent restarts.
- **`influx`** is InfluxDB line protocol, which Grafana Cloud's Mimir ingests
  as Prometheus series named `<measurement>_<field>`: `buzzfleet_heartbeat_up`,
  `buzzfleet_host_load1`, `buzzfleet_host_mem_available_bytes`,
  `buzzfleet_agent_healthy{agent}`, `buzzfleet_agents_ok`, and so on. Each
  agent's state is an enum, `buzzfleet_agent_state_active{agent,state}`: 1
  for the current state and 0 for every other on each beat. A state carried
  as a label would leave the old series answering queries for several
  minutes after every change.
  A silence alert is `absent_over_time(buzzfleet_heartbeat_up{host="…"}[5m])`.

Auth is basic (`basicUser` + `passwordFile`) or a bearer token
(`bearerTokenFile`), and needs https unless the endpoint is on localhost. Logs
show only the endpoint's scheme and host, because webhook URLs often carry a
secret in the path or query. Secrets are read from files on every send, so they never
sit in fleetd.json, and rotating one needs no restart. `intervalSecs` defaults
to 60.

The heartbeat does not wait on the relay. It reports the last health pass with
its age (`assessed_seconds_ago`) and whether the relay answered (`relay_ok`).
So "host fine, relay down" is a different alert from silence.

```bash
buzzfleet heartbeat          # print the body that would be sent
buzzfleet heartbeat --send   # send one now and report the response
```

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
