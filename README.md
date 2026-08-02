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
buzzfleet add scout --workdir /srv/scratch --invite v2.... --display-name "Scout"
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
doctor              duplicates, permissions, membership
```

Only the owner is obeyed. Short replies land inline; long ones are threaded.

Each host answers to its own name and to a shared `@fleet`, so one message
reaches every machine and each reply is labelled with its hostname.

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
