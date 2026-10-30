> Status: decisions approved by the operator 2026-10-30 (wayfinder map claude-aq9, tickets .1 to .5); the theme sections are proposals held in the map's fog. Build epic gt-6mlik. Charted by morgoth.

# Mordor: the Rings as the town's kill switches, and a retheme in increments

## Scope as decided

The operator set the scope on 2026-10-30. These hold for every bead that cites this document.

- **The Ring first.** The One (below) is the only build this document hands to shaped beads now. Its decisions are final and recorded on the map.
- **Rigs keep their names and bead prefixes.** No rig is renamed.
- **Polecat seat names and the per-rig theme files stay.** Polecats are called orcs in prose and keep the names they have.
- **`gt` stays.** The binary, the `GT_` environment variables, the `gt:` labels, the `gt-` prefix and `~/gt` are untouched. Gorthaur is Sauron's Sindarin name, so the letters already fit.
- **Code identifiers are never renamed** (`polecat`, `overseer`, `rig`, the `mayor/rig` path).
- **Overseer and bird identities stay** until turnover makes a change convenient (a new overseer session, a new bird mission). No bulk rename of handoff beads, clones or worktrees.
- **The repo and module rename**, the `gastown.local` identity domain and the `com.gastown` launchd labels are not planned. They are a line in the map's fog, revisited only after the One and the vocabulary have landed.
- Everything moves in small evolutionary pieces. Each piece is a decided map ticket before it is a bead.

The principle behind the list: theme the actors and places, keep plain English for mechanisms. The Mad Max theme already showed what happens otherwise. Mayor, witness, refinery, deacon and dog are retired roles, yet their words survive in hundreds of Go files and a canonical path. Roles die faster than nouns. Rig, bead, seat, landing, gate, sling and pool stay English.

## The One: the town's power brake

### Why

Today's brakes are sentinel files whose presence means stop, so run is the default: the operator hold (`seat-refill.hold`), the town ESTOP, the per-rig ESTOP, the per-rig dispatch hold record and the parked record. Five brakes in two storage forms. A reboot returns to standby only because a boot job writes the hold file before the first dispatch tick. The One inverts the default: power flows only while a signed lease exists and is valid. A presence token with a lease fails safe on every error class and gives a town that runs unattended overnight a dead-man switch it does not have.

The town's power package (full, standby, off; claude-cl7.15) is the hinge. The One is not a fourth brake beside those states. It is the state variable they were missing.

### Decisions (claude-aq9.1, .2, .3)

| Decision | Resolution |
|---|---|
| Lease | 36 hours. `gt ring renew` extends it from now. Renewal is a deliberate operator act, never automated. `gt power status` and the dashboard show the time to the wane; the daemon logs the wane an hour ahead and when it happens. |
| Storage | A `ring` section of the machine-owned `mayor/town.json` (the two-file layout of D5, gt-y3pgh.7), fields forged_at, expires_at, forged_by, hmac. Written only by the `gt ring` verbs through the kernel writer. A missing section, bad signature or unparseable record reads as absent, with a reason, failing closed. |
| Signing | HMAC-SHA256 over the canonical fields. The key is named by reference in the operator-owned `settings/config.json` (`ring.key` = a `${VAR}` reference) and resolved by the kernel's env expansion from `daemon.env`, never read by `os.Getenv`, never printed, logged, placed in a fixture or an error text. The key is absent from every agent's spawn environment, which is what makes operator-only hold. Every session on this machine runs as the operator's user, so the key is tamper-evidence against buggy or accidental writes, not a security boundary. |
| States | Valid is full. Waned (expired) is standby, exactly today's standby with hold and ESTOP both set: no automatic dispatch, no manual sling, no restarts or kills through the supervisor, landings in flight finish, seats idle out, worktrees kept, housekeeping continues. Absent or badly signed is off: the standby drain, then the power package stops daemon, watchers and Forgejo. Nothing kills. `gt kill-all` stays the one explicit act (gt-4k3fj.4). |
| Power verbs | `gt power standby`, `full` and `off` stay as the operator interface and are implemented on the Ring: standby sets the lease expired now, full renews, off removes the record and stops the parts. |
| One predicate | Standby is a single check every job reads: the heartbeat lifecycle flag, the tier sweep, the scheduled maintenance Dolt gc, the dispatch hold, the sling path and the supervisor guard. The overrun guard stop is routed through the supervisor so it refuses under standby like every other kill. Named exceptions keep running during a drain: the landing worker, polecat health, idle reap, Dolt supervision, log rotation, townhealth and attention, the resource samplers. |
| Reboot | A reboot does not touch the lease. The daemon treats infrastructure not yet healthy (Docker, Forgejo, Dolt) as standby regardless of the Ring and moves to full once the parts answer. The boot hold job and its plist are deleted. This keeps the intent of claude-cl7.15 Q7 (health first) with a different mechanism, and the lease is the operator's choice. |

### What the research found (claude-aq9.4)

Of 53 daemon jobs, 28 read neither brake; 21 read the ESTOP only through the lifecycle flag; 3 read the hold. During a drain the hold is on and the ESTOP is not yet, so 22 lifecycle steps still run. The landing worker ignores both by design. The overrun guard stop kills sessions through tmux even under ESTOP, bypassing the supervisor. A scheduled Dolt gc can start after the drain saw none. The tier sweep files beads under both. The table is `2026-10-30-drain-writers.md` beside this file.

### Build slices (claude-aq9.5; epic gt-6mlik)

1. `internal/ring`: record, sign and verify, kernel read and write, state, a `ringtest` helper. Read by nothing.
2. `gt ring forge`, `renew`, `status`, `remove`. Key by reference, refuse without it, never print it.
3. `gt power status` prints the Ring state beside the sentinels. No behaviour change.
4. Overrun guard stop routed through the supervisor. A standalone bug fix.
5. Readers honour the stricter of Ring and sentinels: dispatch hold, sling path, supervisor Restart and Kill guard.
6. One standby predicate for the lifecycle flag, the tier sweep and the maintenance gc.
7. Power verbs implemented on the Ring; boot gates on infra health.
8. Proving step on the live town with the sentinels in place: forge, wane to standby, renew to full, one off. Operator and overseer, not a polecat. The overseer asked for it; it is the condition for slice 9.
9. Delete the hold file, the town ESTOP sentinel, the boot hold job and plist, and the town forms of `gt estop` and `gt thaw`, in one landing. No legacy fallback after it. Rig ESTOP, rig hold and park are untouched.

Install trap: nothing reads the Ring for behaviour until slice 5, and slice 5 honours the sentinels, so an unforged Ring cannot stop the town before slice 9. Handbook and overseer-check wording change with slices 5 to 7 (claude-7we) so no instruction points at a file that is about to go; the runbook and the rest follow (claude-aq9.10).

## The Nine: bird identity tokens (decided in principle, waiting on WF6)

A bird's identity today is environment variables anyone can set; a lost variable writes as the operator, and the landing worker's refusal of the human actor (gt-2ge3z, gt-i817y) patches one hole at a time. The tracker program decided the mechanism (WF6, claude-qpy.6): one bearer token per seat, issued by gt at seat creation, hashed in a tokens table, revoked on removal, identity replacing the actor variable. **The Nine are WF6 tokens for bird seats.** The only gap is that birds are started by the operator's launcher, so nobody issues theirs. The ticket (claude-aq9.7) is blocked until WF6 lands; the recommendation is that the launcher asks gt to issue the token and the overseer's revoke is the WF6 revoke. A revoked bird is silenced, not stopped (claude-aq9.8, recommendation): the session keeps its context and can hand off once the ring is restored. Whether nine is a cap on long-lived named sessions is claude-aq9.6.

Glossary for this effort: **the One** is the town power lease; **a ring** is a bird's identity token and nothing else; the rig-level run record keeps its word, **park**.

## The theme (proposals, held in the map's fog)

None of this is scheduled. It is recorded so the words stay consistent when a piece becomes convenient. The vocabulary ticket (claude-aq9.9) decides which words enter docs and log prefixes; code identifiers, config keys, rig names and seat names never change.

| Today | Name | Note |
|---|---|---|
| the operator | Morgoth | the one above Sauron; a morgoth session is the operator's design voice, outside the town, no ring, no town writes |
| the overseer | Sauron | as a session name only, at turnover; the role word overseer stays in labels and code |
| the nine birds | the Nazgûl | canon names two (Angmar, Khamûl); the rest are roleplaying lore (Dwar, Indûr, Akhôrahil, Hoarmûrath, Adûnaphel, Rên, Ûvatha); names pass with the mission seat, at turnover |
| polecats | orcs | prose only; seat names unchanged |
| om review | the Watchers | the Two Watchers at Cirith Ungol guard a gate and refuse passage |
| hm (history-man) | the palantír | the stone you ask and it shows what was recorded elsewhere |
| the daemon | the Tower (Barad-dûr) | the one process that never sleeps |
| spec dispatcher, landing worker, reaper, tier sweep | the Whip, the Gate-warden, the Carrion, the Searchers | log prefixes at most; config keys stay |
| observability | the Eye | whether this names mango the rig or the whole surface is part of claude-aq9.9 |
| escalations and asks | the Mouth of Sauron | the herald who speaks for the Tower to the one above it |
| the town, Forgejo, the k8s cluster, devops | Mordor, the Morannon, Gorgoroth, Sammath Naur | places; devops is where the One is forged, the one rig that already needs the operator's own hand |

Rename, if it ever comes, is in place, never a new repo: a new repo loses the om verdicts in `refs/notes/om`, the hm commit index and every bead-to-commit link. om and hm stay separate repos.

## Standing decisions this document defers to

- Power states and their orderings: claude-cl7.15 (2026-10-10), the power package, `docs/power-runbook.md`.
- E-stop stops dispatch and restarts only; kill-all is separate: gt-4k3fj.4.
- Config kernel: one source per fact, machine-owned records written by gt verbs, secrets by reference, identity-only env: D5 (claude-1ey.5, gt-y3pgh); one parked record: gt-y3pgh.4.
- Per-rig dispatch hold without park: gt-vty1v.
- Bearer tokens per seat: WF6, claude-qpy.6.
- Landing refuses the human actor: gt-2ge3z, gt-i817y.
