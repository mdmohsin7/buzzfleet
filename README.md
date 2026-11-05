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
doctor              duplicates, permissions, membership, enforced limits
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
`channels`, `limits`, `install` and `init`. Run `buzzfleet` for the full list.

## Health

| state | meaning |
|---|---|
| `ok` | running, present on the relay, agent process alive |
| `wedged` | running, but the relay cannot see it |
| `stalled` | present on the relay, but its agent process is gone |
| `down` | not running |
| `unknown` | the relay was unreachable — never blamed on the agent |

`wedged` and `stalled` are healed automatically after two consecutive
confirmations, and the supervisor says what it did in the channel. It stops
after three restarts an hour and tells you once, because an agent a restart
cannot fix should not be restarted forever.

## Resource limits

A small host running several coding agents is one parallel test suite away
from a load of 50, and then sshd, the supervisor and every agent crawl
together. `buzzfleet limits` sizes a ceiling for all the agents together from
the host's cores and RAM, keeping half a core and an eighth of RAM (at least
1 GiB) for the host itself:

```
$ buzzfleet limits
host        2 cores, 11941M RAM, cgroup hybrid
agents      150% CPU and 10449M memory between them
containers  1 CPU and 2985M memory each, via the supervisor's Docker proxy

  sudo systemctl set-property user@1001.service CPUAccounting=yes MemoryAccounting=yes CPUQuota=150% MemoryLimit=10449M
```

`--apply` runs that command. Where the ceiling goes depends on the host:

- **cgroup v2 with cpu and memory delegated** to your user manager: on
  `buzz-agent.slice`, which holds exactly the agents. No root needed.
- **cgroup v1, or v2 without delegation** (Ubuntu 20.04 and older): on the
  system unit `user@UID.service`, with sudo. That holds every user unit, the
  supervisor included, but not SSH sessions, so a saturated fleet still cannot
  lock you out. Per-agent limits cannot work here at all: systemd accepts a
  `MemoryMax=` in an agent's drop-in and silently ignores it.

Because systemd accepts limits it cannot apply, `doctor` reads the kernel's own
cgroup files for each agent and says which ceiling actually binds:

```
ok   dexy: enforced CPU 150% (on user@1001.service), memory 10449M (on user@1001.service)
WARN dexy: its unit sets MemoryMax=1600M but that is NOT enforced
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
that asks for more; a smaller request is kept as asked. It also labels each
container `buzzfleet.agent=<name>` and `buzzfleet.created=<unix time>`. Compose,
SDKs and the CLI all go through it, interactive and streaming sessions included.

This is a guardrail, not a security boundary: an agent in the `docker` group can
still reach `/var/run/docker.sock` directly. `docker build` steps run inside
BuildKit rather than as containers, so they are not capped. Set
`"noDockerProxy": true` under `limits` to turn the proxy off. An agent picks up
the proxy when it next restarts, and `doctor` lists the ones that have not.

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
