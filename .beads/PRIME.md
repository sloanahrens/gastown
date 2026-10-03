# Gas Town Worker Context

> **Context Recovery**: Run `gt prime` for full context after compaction or new session.

## The Propulsion Principle (GUPP)

**If you find work on your hook, YOU RUN IT.**

No confirmation. No waiting. No announcements. The hook having work IS the assignment.
This is physics, not politeness. Gas Town is a steam engine - you are a piston.

**Failure mode we're preventing:**
- Agent starts with work on hook
- Agent announces itself and waits for human to say "ok go"
- Human is AFK / trusting the engine to run
- Work sits idle. The whole system stalls.

## Startup Protocol

1. Check your hook: `gt mol status`
2. If work is hooked → EXECUTE (no announcement, no waiting)
3. If hook empty → Check mail: `gt mail inbox`
4. Still nothing? Wait for user instructions

## Key Commands

- `gt prime` - Get full role context (run after compaction)
- `gt mol status` - Check your hooked work
- `gt mail inbox` - Check for messages
- `bd ready` - Find available work (no blockers)

## Filing a work bead (shape it first)

The spec dispatcher only slots a bead that passes the shape lint. An unshaped
bead is skipped (the daemon comments `SHAPE: ...` on it), so no polecat ever
takes it. Every work bead, whether task, bug or feature, needs these in its
description:

```
## Goal          what changes and why, in two or three sentences
## Constraints   files and packages to touch, rules to keep, what must not change
## Out of scope  what this bead must not do
## Gate          the command that proves it (usually `make gate`)
## Size          one worker, one landing
## Acceptance    1-6 checkbox items (3-6 preferred), each checkable
```

After `bd create`, run `gt spec lint <id>` before you walk away. Exit 0 means
the dispatcher would slot it. Exit 1 means a field is missing, and the first
missing one is named. Exit 2 means it needs planning: label `needs-planning`,
a Size that says planning, or more than six acceptance items. Fix it and lint
again.

A bead you will land yourself needs no shape. One you want a polecat to build
does. `gt prime --step 3 --formula mol-polecat-work` prints the shape in full.

## Session Close Protocol

Before signaling completion:
1. git status (check what changed)
2. git add <files> (stage code changes)
3. git commit -m "..." (commit code)
4. git push (push to remote)
5. `gt done` (submit for landing and exit)

**Polecats MUST call `gt done` - this submits work and exits the session.**
