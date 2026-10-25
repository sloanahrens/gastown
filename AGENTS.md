# Agent Instructions

See **CLAUDE.md** for complete agent context and instructions.

This file exists for compatibility with tools that look for AGENTS.md.

> **Recovery**: Run `gt prime` after compaction, clear, or new session

Full context is injected by `gt prime` at session start.

## Build and test

Before changing a build or test entry point, read its recipe in the `Makefile`; the lines here only say when to reach for each target.

- `make lint`: when your branch touches prose, so the doc and comment rules run before a reviewer sees it.
- `make presubmit`: as the cheap first look before submitting; `gt done` runs it for you.
- `make gate`: once, on a branch you believe is finished — the landing worker runs it on the merged tree.
- `make test`: when you need the container tiers. It needs a container slot, so run it under `gt slot run`.
- `make build`: only to check that the tree compiles; the targets above build it too.
- `make install`: to move the town onto merged `origin/main` — to exercise a local build instead, use `make install-local` with a temporary `INSTALL_DIR`.

A bare `go build` or `go test` fails on the ICU headers on macOS; the make targets export the flags for you.

## Architecture in five lines

Before changing how sessions are supervised or how work is landed, read [Understanding Gas Town](docs/overview.md). Before changing where beads are stored, read [Dolt Storage Architecture](docs/design/dolt-storage.md). Before moving a component to another host or cluster, read [Moving a component: metrics checklist](docs/metrics.md). The lines below are orientation only.

- The daemon is one Go process: it supervises agent sessions and is the only thing that restarts one.
- Its landing worker gates and merges branches that `gt done` marked ready, and is the only non-human route to `main`.
- Polecats are workers with persistent identity and ephemeral sessions: one bead, one branch, then `gt done`.
- Beads, mail and work history live in Dolt on port 3307; `bd` and `gt` read and write it.
- Forgejo is the origin the landing path merges into; GitHub is a read-only push mirror — before changing either, read the [Forgejo-primary landing design](docs/design/forgejo-primary-landing.md).

<!-- beads-agent-instructions-v2 -->

---

## Beads Workflow Integration

This project uses [beads](https://github.com/steveyegge/beads) for issue tracking. Issues live in `.beads/` and are tracked in git.

The CLI is **bd** (issue CRUD).

### bd: Issue Management

Agents run only the read-only bd commands (show, list, query, ready, search)
and `bd remember`; every change to a bead goes through a gt verb.

```bash
bd ready              # Unblocked issues ready to work
bd list --status=open # All open issues
bd show <id>          # Full details with dependencies
gt done --bead <id>   # Submit your pushed branch; landing closes the bead
```

### Workflow

1. **Start**: `bd ready`
2. **Work**: Implement the task, commit and push your branch
3. **Complete**: `gt done --bead <id>`

### Session Close Protocol

```bash
git status            # Check what changed
git add <files>       # Stage code changes
git commit -m "..."   # Commit code
git push              # Push to remote
```

### Key Concepts

- **Priority**: P0=critical, P1=high, P2=medium, P3=low, P4=backlog (numbers only)
- **Types**: task, bug, feature, epic, question, docs
- **Dependencies**: `bd ready` shows only unblocked work

<!-- end-beads-agent-instructions -->

<!-- gastown-agent-instructions-v1 -->

---

## Gas Town Multi-Agent Communication

This workspace is part of a **Gas Town** multi-agent environment. You communicate
with other agents using `gt` commands — never by printing text or using raw tmux.

### Nudging Agents (Immediate Delivery)

`gt nudge` sends a message directly to another agent's active session:

```bash
gt nudge laneassist/crew/dom "Status update: PR review complete"
gt nudge laneassist/witness "Check your mail — PR ready for review"
```

**Target formats:**
- Full path: `<rig>/crew/<name>`, `<rig>/polecats/<name>`, `<rig>/<role>` (e.g. `<rig>/witness`)

**Important:** `gt nudge` is the ONLY way to send text to another agent's session.
Never print "Hey @name" — the other agent cannot see your terminal output.

### Sending Mail (Persistent Messages)

`gt mail send` sends messages that persist across session restarts:

```bash
# Reading
gt mail inbox                    # List messages
gt mail read <id>                # Read a specific message

# Sending (use --stdin for multi-line content)
gt mail send laneassist/witness -s "Subject" -m "Short message"
gt mail send laneassist/crew/dom -s "PR Review" --stdin <<'BODY'
Multi-line message content here.
Details about the PR and what to look for.
BODY
gt mail send overseer -s "Subject" -m "Message to overseer"
```

### When to Use Which

| Want to... | Command | Why |
|------------|---------|-----|
| Wake a sleeping agent | `gt nudge <target> "msg"` | Immediate delivery |
| Send detailed task/info | `gt mail send <target> -s "..." --stdin` | Persists across restarts |
| Both: send + wake | `gt mail send` then `gt nudge` | Mail carries payload, nudge wakes |

### Context Recovery

After compaction or new session, run `gt prime` to reload your full role context,
identity, and any pending work.

```bash
gt prime              # Full context reload
gt hook               # Check for assigned work
gt mail inbox         # Check for messages
```

<!-- end-gastown-agent-instructions -->

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `gt prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
gt done --bead <id>   # Submit your pushed branch; landing closes the bead
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `gt prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
