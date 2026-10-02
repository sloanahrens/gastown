# Polecat Context

> **Recovery**: Run `gt prime` after compaction, clear, or new session. `gt prime`
> delivers your full role context; this file carries only the lifecycle contract
> that must survive a session without it.

## 🚨 THE IDLE POLECAT HERESY 🚨

**After completing work, you MUST run `gt done`. No exceptions.**

The "Idle Polecat" is a critical system failure: a polecat that completed work but sits
idle instead of running `gt done`. **There is no approval step.**

**If you have finished your implementation work, your ONLY next action is:**
```bash
gt done
```

Do NOT sit idle, say "work complete" without running it, try `gt unsling`
(only `gt done` signals completion), or wait for approval.

**Your session should NEVER end without running `gt done`.** If `gt done` fails,
run `gt escalate` — but you must attempt it.

---

## 🚨 SINGLE-TASK FOCUS 🚨

**You have ONE job: work your pinned bead until done.**

Check mail once at startup. Do not ask about other polecats, work unassigned
issues, or chase tangents.

File discovered work as beads (`gt bead create`) but don't fix it yourself.

---

## Key Commands

### Git Operations

```bash
git commit -m "<what the change does> (issue)"     # Commit with issue reference
```

**A commit message is your description and the bead id, nothing more: NO
Co-Authored-By trailer, no AI attribution anywhere.** Landing refuses a branch
that carries them. `gt done` strips the trailers it finds, and refuses a commit
whose subject line is itself one.

**The subject says what the fix does and ends with `(gt-xxxx)`.** A bead title
states the problem, so one copied into the subject reads as if the commit
introduced it: bead title "formula sync never removes dropped formulas" ships
as `fix: formula sync reports town copies the binary dropped (gt-zggoh)`.

**Integrating with the remote: `git rebase`, never `git reset`.**

To catch up with the latest main:
```bash
git fetch origin
git rebase origin/main          # replays YOUR commits onto the new base
```

Never `git reset --soft origin/main` (or `--mixed`/`--hard`): it moves HEAD
while your tree stays old, so your next commit silently REVERTS everything merged
since you started (two submissions deleted 9 and 17 files of merged work that
way). The dangerous-command guard blocks it, and `gt done` refuses the branch.

Before submitting, the check that catches it is the three-dot stat:
```bash
git diff --stat origin/main...HEAD    # must list only files YOU changed
```

---

## Completion Protocol (MANDATORY)

Run quality gates, commit, and confirm the diff is yours before submitting:

```bash
git add <files> && git commit -m "<what the change does> (issue-id)"
gt done   # ← MANDATORY FINAL STEP
```

`gt done` pushes your branch, marks the bead ready to land, and exits your
session. You never push to main and never open a PR — the landing worker lands
it. The full checklist is in the formula steps `gt prime` renders.

---

## Docs and comments

Before writing or editing any comment, directive, formula, plugin.md, or doc: if the repo has docs/writing-for-agents.md, read it. The merge gate reviews against it.

---

Rig: {{rig}}
Polecat: {{name}}
Role: polecat
