# One agent in several communities

Status: **design, for review**. Nothing here is built yet.

## Goal

An agent that has built up knowledge in one community (files in its workdir,
the coding agent's own memory, Buzz memory, chat history) should be able to
serve a second community with that same knowledge, under the same identity.

Out of scope: one agent spread across several hosts. Its harnesses, its workdir
and its model login all stay on one host. Splitting them would mean syncing the
workdir and the login between machines. That is a different and much harder
problem, and nothing asked for here needs it.

## What the upstream code says

These facts were checked in block/buzz rather than assumed. They shape every
section below.

- **One relay per harness.** buzz-acp connects to the single relay in
  `BUZZ_RELAY_URL`. So "in two communities" means two harness processes under
  one agent definition.
- **One turn at a time today.** By default a harness runs one coding-agent
  subprocess (`BUZZ_ACP_AGENTS=1`) and queues events while it is busy. Today an
  agent never runs two turns at once. A second harness on the same workdir would
  break that by default.
- **Identity is portable, membership is not.** The same key can join any number
  of communities. Each community needs its own invite claim, and each relay keeps
  its own membership, owner registry and presence.
- **Buzz memory (NIP-AE) is scoped to (agent, owner), not to a community.**
  - Engrams are addressable `kind:30174` events signed by the agent.
  - They are encrypted to the NIP-44 conversation key of the agent and its owner.
  - The `d` tag is an HMAC of the slug under that same key.
  - A memory "lives in a community" only in the sense that it was published to
    that community's relay.
  - Consequences:
    - With the **same owner** in both communities, the same signed events are
      valid on both relays. They can be copied verbatim, with no re-signing and
      no re-encryption.
    - With a **different owner**, the namespace is different. Copying means
      decrypting and re-encrypting for the second owner, and that owner can then
      read everything that was copied.
  - Deletion is an in-band tombstone (`value: null`) at a newer `created_at`.
    Head selection is last-writer-wins by `created_at`, with ties broken by the
    lowest event id.
  - The relay validates the envelope, serves engrams only to their author or the
    `p`-tagged owner, and refuses a write that an existing newer head dominates.
- **buzz-acp injects only the `core` engram,** once per new session. It never
  writes memory itself.
- **The coding agent keeps its own memory, keyed by working directory.** Claude
  Code keeps per-project memory and transcripts under its config dir, keyed by
  the absolute path of the workdir. Two harnesses on the same path share that
  memory automatically. On the deployment that motivated this design, that
  memory plus the workdir *is* the accumulated knowledge. The agent had no
  NIP-AE engrams at all.
- **Path restrictions in systemd user units are not enforced everywhere.** On an
  Ubuntu 24.04 host with AppArmor's unprivileged-userns restriction on:
  - `InaccessiblePaths=` and `ReadOnlyPaths=` in a **user** unit are accepted
    and silently ignored, with or without `PrivateUsers=yes`. The file stayed
    readable and writable.
  - The cause is in the kernel log: AppArmor's `unprivileged_userns` profile
    denies `CAP_SYS_ADMIN` inside the user manager's namespace. systemd then
    skips the mount namespace without a word.
  - That makes **every** namespacing option in user units (`ProtectProc=`,
    `ProtectHome=`, and so on) a silent no-op on such hosts.
  - The same properties on a **system** unit with `User=` are enforced (tested).
- **Same uid means no secrets.** Any process running as the agent's Linux user
  can read `/proc/<pid>/environ` of every other process of that user. That
  includes every agent's `BUZZ_PRIVATE_KEY`.
  - Yama `ptrace_scope=1` does not stop it: reading `environ` only needs
    `PTRACE_MODE_READ`, and Yama gates attach.
  - `ProtectProc=` cannot help either. It needs systemd ≥ 247, which the
    20.04-era hosts lack. On newer hosts it is dead in user units for the reason
    above.
- **Git credentials are per relay.** buzz-acp gives each harness an ephemeral
  git identity through `GIT_CONFIG_*` environment variables, scoped to its own
  relay's git endpoint. Nothing is written to repository config, so sharing a
  workdir is safe. But a repo whose remote is on community A's relay cannot be
  pushed from community B's harness: B holds no credential for A's relay.

## 1. Data model

`agent.json` gains a `communities` map. The existing top-level fields stay and
keep describing the **primary** community, so the file is still valid for older
binaries.

```json
{
  "name": "agent-a",
  "workdir": "/srv/agent-a",
  "agentCommand": "claude-agent-acp",
  "relay": "wss://relay-a.example",
  "owner": "<hex>",
  "respondTo": "owner-only",
  "communities": {
    "b": {
      "relay": "wss://relay-b.example",
      "owner": "<hex>",
      "respondTo": "owner-only",
      "access": "full",
      "allow": [],
      "joinedAt": 1767225600
    }
  }
}
```

- The primary community is always named `main` and comes from the top-level
  fields. `communities` lists only the others. Names are `[a-z0-9]{1,16}`, with
  no dashes, because a dash separates the agent from the community in unit names.
- **Per-community settings:**
  - `relay`, `owner` and `respondTo`: the owner can differ per community, and
    `--agent-owner` and owner-only replies follow it.
  - `access`: `full` or `restricted` (see section 5).
  - `allow`: the restricted allowlist.
  - Every other community of the agent is readable through `peek` (section
    4b). Limiting that per community belongs with `restricted` access.
- **Invites are not stored.** They are single-use secrets. `joinedAt` records
  that the claim succeeded. Channels are not stored either; they are queried
  live, as today.
- **One entry per relay.** Relays are compared in canonical form, and a second
  community entry on a relay already used is refused at config time. Two
  harnesses on one key and one relay double-reply, so this is caught before
  `doctor` ever has to.
- **Migration** needs no rewrite. An `agent.json` with no `communities` is an
  agent in `main` only, as today. The file is only rewritten by
  `community add`/`rm`, and that rewrite keeps the top-level fields byte-for-byte.
  That needs a key-order-preserving JSON round-trip, which Go's map marshalling
  does not give. It is built and tested as such.
- **Rollback:**
  - An older binary ignores `communities` and keeps running the primary harness.
  - It cannot see the extra harnesses, so rolling back is
    `buzzfleet community rm <agent> <name>` for each one first. That stops and
    disables the extra unit, then the binary swap.
  - The doc and `doctor` both say this.
  - Running an extra unit's `run-agent` under an old binary fails closed: there
    is no such agent named `agent-a--b`.

## 2. Process model

- **The primary harness is unchanged:** `buzz-agent@<agent>.service`, same
  template and same drop-ins. Existing agents are not restarted just to keep
  working.
- **Each extra community gets `buzz-agent@<agent>--<community>.service`,** from
  the same template.
  - `run-agent` splits the instance name on `--` and sets `BUZZ_RELAY_URL`,
    `--agent-owner` and `--respond-to` from that community.
  - Agent names are already `[a-z0-9-]` and will be forbidden from containing
    `--`.
  - Per-agent drop-ins (model, env, queue mode) are linked: `community add`
    symlinks each `.conf` from the primary's `.d/` into the community unit's
    `.d/`. So the harnesses cannot drift apart, and the primary stays the one
    place to edit. `doctor` flags a primary drop-in that is not linked.
  - The template-wide drop-in (`Nice=`, and so on) applies to both as it
    already does.
  - The unit is in `buzzfleet.slice`, so the fleet ceiling covers it.
- **Restricted communities run as a system unit** instead (see section 5),
  because only system units get enforced namespaces.
- **Addressing:** commands accept `agent` (every harness of that agent) or
  `agent@community` (one harness).
  - `status` and `health` print one row per harness: `agent-a@main`,
    `agent-a@b`.
  - `restart agent-a` restarts every harness. `restart agent-a@b` restarts one.
  - `logs agent-a@b`, `channels agent-a@b` and `join agent-a@b <uuid>` work the
    same way.
  - In chat, `@fleet restart agent-a@b` works.
- **Health and presence** are per harness. Presence is read from each
  community's relay as the agent itself (the agent is a member there, and the
  supervisor may not be). The heal and give-up logic is keyed by harness.
- **A harness waiting on the turn gate is never healed.** The gate records the
  current holder (harness and prompt start time) in the lock file. `status` and
  `health` show `waiting on agent-a@main since 14:02`. A long or wedged turn is
  the holder's problem, so restarting the waiter would fix nothing.
- **Supervisor notices** about a non-primary harness name it by its local name
  (`agent-a@b`). They never include B's relay URL, members or channels, because
  they are posted in the primary community.
- **Duplicate detection is keyed by (pubkey, canonical relay),** read from each
  `buzz-acp` process's environment, not by pubkey alone. Two harnesses on one
  key in different communities are correct. Two on one key in the *same*
  community double-reply, and `doctor` and `reap` catch that.
- **Docker:** one proxy socket per harness. Containers get labelled
  `buzzfleet.agent` and also `buzzfleet.community`.
- **The supervisor** stays in the primary community's control channel and is
  the only one. Notices about any harness go there.

## 3. Concurrency on the shared workdir

**Choice: one turn at a time per agent, across all of its communities,
enforced by a gate between buzz-acp and the coding agent.**

`run-agent` sets the agent command to `buzzfleet acp-gate --agent <agent> --
<real command>`. The gate passes ACP JSON-RPC through unchanged, except:

- When it sees a `session/prompt` request, it takes an exclusive `flock` on the
  agent's turn lock before forwarding. It releases the lock when the matching
  response comes back and no other prompt from this harness is still in flight.
- **Where the lock lives:** `<FLEET_HOME>/run/turn-<agent>.lock`, in a directory
  owned by a per-agent group (mode 2770). Every harness of the agent can open it,
  including a restricted one running as another Linux user, which gets it bound
  into its namespace. A lock under `$XDG_RUNTIME_DIR` (mode 0700) would silently
  leave restricted turns unserialised.
- **The holder is recorded in the lock file:** which harness, and when its
  prompt started. Waiters and `status` read it.
- **While a prompt holds the lock,** every agent→client request passes through
  untouched: `fs/*`, `session/request_permission`, terminal. So do all
  notifications.
- Within one harness it is re-entrant, so buzz-acp's own steering or queueing
  cannot deadlock against itself.
- `session/cancel` is a notification, so the gate cannot answer it. For a prompt
  still waiting for the lock, the gate instead replies to that pending
  `session/prompt` with `stopReason: cancelled`, and nothing reaches the coding
  agent.
- If the harness or the coding agent dies, the kernel drops the flock. A crash
  cannot wedge the other community.
- **The queued message:** after about 60 s of waiting, the gate sends one short
  `agent_message_chunk` for the waiting prompt: "Busy with another task — this
  is queued and I'll get to it." It says the agent is busy, not where.
- **The wait limit is the turn limit,** 2 h by default, because waiting behind
  legitimate work is normal, not an error. Only if the wait outlives the turn
  limit does the prompt fail with an error naming the cause.

**Why this one:**

- It keeps the invariant that holds today: one agent, one turn at a time. Work
  cannot be corrupted by two turns editing the same files, the same git
  checkout or the coding agent's memory files at once, because there are never
  two turns.
- It enforces that in code, without asking the model to behave.

**Rejected:**

- **Git worktree per community.** The workdir is not one repository. Typically
  it is many repos plus notes, data and scratch. Worktrees would only isolate
  the git parts. They would also split the per-path coding-agent memory, which
  is the knowledge this feature exists to share.
- **Per-community scratch dirs plus file locks.** The locks are advisory: they
  depend on the model taking them on every edit. A per-community scratch dir is
  still offered as `BUZZ_FLEET_SCRATCH` for temporary output.

**Cost:** while the agent works for one community, the other waits. An agent
that takes 70-minute turns makes the second community wait up to 70 minutes. The
harness still shows typing and liveness while it waits. Whether that is
acceptable, or whether some communities should be able to preempt others, is an
open question (section 10).

**Not gated:**

- Background processes a turn leaves running after it ends. They are the
  agent's own, as today.
- Each harness's idle coding-agent process writing its own config and history
  files (Claude Code's `~/.claude.json`) outside a turn. Agents sharing a config
  dir already do this today, so it is not new.

**Also gated:** the supervisor's periodic memory sync takes the same lock, so it
never runs during any of the agent's turns.

## 4. Sharing knowledge

### (a) Buzz memory

`buzzfleet memory sync <agent> --from main --to b [--both] [--exclude <glob>]... [--include-core] [--dry-run]`

- **Same owner on both sides:**
  1. List both relays as the agent. Fetch every head and validate it per NIP-AE.
  2. For each slug not excluded, pick the head by NIP-AE rules (greatest
     `created_at`, then lowest id).
  3. Republish **that exact signed event** to each side whose head is older or
     missing. Nothing is re-signed or re-encrypted, so the other side holds a
     byte-identical copy.

  The relay's own dominance check means a stale copy can never overwrite a
  newer head.
- **Tombstones** are ordinary events, so a deletion propagates like any other
  write. Excluded slugs are never read from or written to the other side, so a
  community-private memory stays where it is. That also means a deletion of a
  private slug never crosses.
- **Direction:** one-way (`--from` → `--to`) by default. `--both` is
  bidirectional.
- **Conflicts:** last-writer-wins would silently drop one side's edit when a slug
  changed on both sides since the last sync. On the **first** sync of a pair,
  there is no record yet. A slug present on both sides with different heads is
  then a conflict too, not last-writer-wins. Sync keeps a local record of the
  last event id it saw per slug and per pair. If both sides moved past it, sync
  writes nothing for that slug. It reports a conflict and saves both values to
  `agents/<agent>/memory-conflicts/` until
  `--resolve <slug>=from|to` is given. Those files and the per-pair record hold
  decrypted memory, so they are created 0600 in a 0700 directory.
- **`core` is excluded by default.** It holds the agent's identity and rules,
  and those can rightly differ per community.
- **Different owners:** a verbatim copy is impossible. Copying needs
  `--reencrypt-for-owner`, together with an explicit `--only <glob>` list, and is
  refused otherwise. That owner can then read everything copied, and the command
  says so before it runs.
- **Ongoing sync:** off by default. With `memorySync: {"to": [...], "exclude":
  [...], "everyMins": N}` in `agent.json`, the supervisor runs the same code
  periodically. It does not run it while the agent is mid-turn.

### (b) Reading the other community's history

- **How the agent learns about the other communities:**
  - The env var `BUZZ_FLEET_COMMUNITIES`: JSON with each community's local name,
    marking the one this harness answers in. No relay addresses.
  - A generated section in the system prompt, naming the communities and the
    tool below.
- **The tool:** `buzzfleet peek <community> <buzz subcommand...>`. It sets that
  community's relay and allows only read subcommands: `messages
  get|thread|search`, `channels list|get|search|members`, `feed get`, `users
  get|presence` and `mem ls|get|hash`. Anything else is refused, as are the
  flags that change the relay or identity. Every other community of the agent
  can be read; limiting that belongs with `restricted` access.
- **Peek's environment is an allowlist,** not the harness's own. The buzz CLI
  sends `BUZZ_AUTH_TAG` with every request, so the harness's environment would
  hand one community's owner attestation to another's relay. It always reads
  with the agent's own key, never a key the caller has exported.
- **Auth tags are per community.** An owner attestation (`BUZZ_AUTH_TAG`) must
  never sit in the per-agent `identity.env`, because every community's harness
  would then present the main community's. If attestations are ever used, they
  belong in each community's own settings.
- **Honest limit:** this is advisory. The harness has the agent's key and the
  `buzz` CLI. It could set `BUZZ_RELAY_URL` itself and post into the other
  community, because the agent is a member there. Making it truly read-only
  would take a key that is not a member of the other community, which defeats
  "same identity". The prompt says to use `peek` and never post across.
- **Git across communities:** a repo whose remote lives on A's relay can be
  read from B's harness, because it is the same workdir. It cannot be pushed
  from there: B's harness only holds a git credential for B's relay. Pushes to
  A's repos belong to A's harness.

### (c) A shared notes file

`agents/<agent>/shared-notes.md` is passed to every harness as
`BUZZ_FLEET_SHARED_NOTES`. The addendum tells the agent to:

- read it when a session starts;
- append durable facts that should help in every community;
- never put one community's private details in it.

In restricted mode it is the one path always bind-mounted read-write. It is a
plain file, not synced anywhere.

## 5. Access control

Every community is added with an explicit `--access full|restricted`. There is
no default, because the right answer depends on the community.

**`full`.** Same Linux user, same workdir path, same coding-agent memory.
Everything the agent can see in its primary community, it can see and repeat in
the other one. The only controls are the system-prompt addendum ("do not bring
X into community Y") and what the operator chooses to keep out of the workdir.
These are **advisory**, and this mode is for communities trusted with all of the
agent's data. That includes A's **raw message history**, not just distilled
knowledge: both harnesses run on the same path, so their coding-agent
transcripts land in the same project directory. An agent can search its own
transcripts and quote A's messages verbatim in B.

**`restricted`.** It restricts **host files only, not community A's data on its
relay.** The harness must hold the agent's private key, because it is the same
identity, and it has outbound network. So it can connect to A's relay as the
agent and read everything the agent can read there:

- A's channels and message history;
- the agent's Buzz memory for every owner.

What `restricted` protects is the workdir, the coding agent's local memory and
transcripts, other agents' keys and the host. Locking the harness to B's relay
and the model API with `IPAddressAllow=`/`IPAddressDeny=` would close the relay
path. It is not proposed, because those addresses move behind CDNs and the rules
would break silently. If A's relay data must be kept from B, the only sound
answer is a **separate identity** for B: a different agent, sharing selected
files.

The host-file part is enforced by layers. All three are needed: the first two
alone leave the other agents' keys readable through `/proc`.

1. **A separate Linux user** per restricted harness, `buzz-<agent>-<community>`.
   - It cannot read other users' processes, so other agents' keys and env are
     out of reach.
   - It cannot read files outside what it is granted.
   - Granted paths come from `allow` entries (`path` or `path:rw`) via POSIX
     ACLs on the agent's files: read by default, write only where marked.
2. **A system unit** with a mount namespace, so the harness sees only what it is
   given even where ACLs are loose:
   - `TemporaryFileSystem=` over the agent's home and the fleet home.
   - `BindReadOnlyPaths=` / `BindPaths=` for each `allow` entry, the shared
     notes file, the tools and the login.
   - `InaccessiblePaths=` for the Docker socket.
   - The coding agent's per-path memory directory for this workdir is bound to a
     per-community directory. So the restricted harness builds its own memory
     unless the primary's memory is in `allow`.
3. **No Docker.** The Docker socket is root in all but name, so a restricted
   harness gets neither the proxy nor the real socket.

**Shared login.** The model login file is bind-mounted read-write and
group-shared, so both harnesses use one account. That matters:

- Token refresh rewrites the file, so copies would drift and log each other out.
- Usage limits then apply to the agent as a whole (section 6).

**Costs:**

- A Linux user and a system unit per restricted harness, so setup needs sudo.
- The supervisor needs a sudoers rule limited to
  `systemctl {start,stop,restart} buzz-community@*` to manage it.

**Verification:** `doctor` proves the restriction from inside rather than
trusting the unit. It runs a probe as that harness, through the same unit
settings, and checks four things:

- a path not in `allow` is unreadable;
- another agent's process environment is unreadable;
- the Docker socket is unreachable;
- `allow` paths are readable.

If any check fails, the harness is refused a start. **Fail closed.**

**Not covered in either mode:** outbound network, and what the agent chooses to
say. A restricted harness can still repeat anything in its `allow` list.

## 6. Shared resources

- **One model login per agent,** shared by all of its harnesses. A usage limit
  or an expired login hits every harness at once, so `auth`/`limited` is a
  property of the **agent**. Transcripts are scanned for the agent as a whole,
  across its per-community memory dirs in restricted mode. The notice names the
  agent once, listing its communities. Restarting one harness does not clear it.
- **Usage:** the turn gate means two communities do not double the agent's
  model usage rate. They share the same one-turn-at-a-time budget.
- **Caps:** every harness sits under the same fleet ceiling. An extra idle
  harness is buzz-acp plus the ACP adapter, a few hundred MB. That is comparable
  to adding an agent, and `limits` should be re-checked on small hosts.

## 7. CLI

```
buzzfleet community add <agent> <name> --invite <code> [--relay <url>]
          --access full|restricted [--allow <path>[:rw]]...
          [--owner <hex>] [--respond-to owner-only|...]
buzzfleet community rm <agent> <name> [--leave]
buzzfleet community ls [<agent>]
buzzfleet memory sync <agent> --from <c> --to <c> [--both] [--exclude <glob>]...
          [--include-core] [--dry-run] [--resolve <slug>=from|to]
buzzfleet peek <community> <read-only buzz subcommand...>   (used by the agent)
```

**`community add`** runs these steps:

1. Claims the invite with the agent's key.
2. Sets the same profile (name, about, picture) in that community.
3. Writes the config.
4. Links the drop-ins.
5. Starts and enables the unit.

It is transactional, like `add`: a failure undoes every step taken.

**`community rm`** stops and disables the unit and removes the config. With
`--leave`, the agent also leaves the community. Its events there stay, because
they belong to that community.

`status` shows `agent@community` rows, and every lifecycle command takes either
form.

## 8. Testing

**Pure tests:**

- Config parsing and migration. A legacy file is `main` only. Round-trips keep
  the top-level fields byte-identical. Invalid names are refused.
- Parsing the agent from a unit instance name.
- Duplicate detection by (pubkey, relay).
- The gate, against a fake ACP agent:
  - two gate processes serialise their prompts;
  - a second prompt from the same harness does not deadlock;
  - notifications pass through untouched;
  - cancelling a prompt that is waiting for the lock answers it without
    reaching the agent;
  - a killed holder releases the lock;
  - the wait limit is enforced.
- The memory sync planner:
  - last-writer-wins and tombstones;
  - exclude globs in both directions;
  - conflicts when both sides moved;
  - `core` excluded by default;
  - different owners refused without the opt-in.
- The `peek` allowlist.

**Integration on a cgroup-v2 test host:** a disposable test agent in two test
communities, with invites supplied by ops. No live agent is touched. The steps:

1. Add the agent in community A.
2. `community add` it to B.
3. Check that both harnesses are present and that a mention in B is answered.
4. Mention it in A and B at once, and check from the gate's log that the turns
   do not overlap.
5. In B, ask about something said in A, so that it uses `peek`.
6. Check that the shared notes file round-trips.
7. Write a test engram, sync it, tombstone it, sync again, and check that an
   excluded slug stays put.
8. Restricted mode, if built: run the `doctor` probe, and try to read a path
   outside `allow` from inside the harness, which must fail.
9. Run `community rm`, and confirm the agent is back to one harness.

## 9. Rollout to an existing deployment

This is a separate step, approved by ops before anything changes on a live host.
In outline:

1. Install the binary.
2. Restart the agent's existing harness once, so that its agent command goes
   through the gate. Without that, its turns are not serialised against the new
   community's.
3. Run `community add` with the chosen access mode.
4. Verify presence, a test mention from the owner, and serialisation.

**Rollback:** `community rm`, then the binary.

## 10. Open questions

1. **How much may the second community see?** Everything (`full`), or an
   allowlist (`restricted`)? `restricted` is several times the work: separate
   users, system units, ACLs, the probe. So it is proposed as a later phase,
   built only if needed. Even then, it keeps host files private but not the
   first community's relay data (section 5). Keeping that private takes a
   separate agent identity.
2. **Who owns the agent in the second community?** The same owner pubkey or a
   different person? That decides who it answers under `owner-only`, and
   whether Buzz memory can be shared verbatim (section 4a).
3. **Is serialising turns acceptable?** It means the second community can wait
   for a long turn in the first one. The alternative is to let the gate preempt
   or interleave, which reopens the concurrency problem.
4. **Persona:** the same system prompt in every community, or a per-community
   addendum (for example, "in this community you are the research assistant")?
5. **Memory sync:** one-time copy, or ongoing? In which direction, and which
   slugs stay private? The motivating agent has no Buzz memory today, so this
   can wait for a real need.
6. **Cross-community chat:** should the agent ever *post* in community A because
   of something asked in B? This design says no. Reading only, through `peek`.

## Proposed order of work

Each phase is its own PR, reviewed before the next starts.

1. Data model, unit naming, per-harness status, health, logs and lifecycle,
   duplicate detection by (key, relay), and `community add/rm/ls` with
   `access: full`.
2. The turn gate.
3. The `peek` tool, the shared notes file and the prompt addendum.
4. `memory sync`.
5. `access: restricted`, only if open question 1 calls for it.

The two-community smoke test runs after phase 3. That is the first point where
the feature is usable end to end.
