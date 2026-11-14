# Polecat no-bead-access: census of seat-side bd and gt calls

Input to the no-bead-access design (approach A: TASK.md in, outbox file out, host applies the outbox at seat end; sandbox seats only). It lists every `bd` and `gt` call a sandbox seat makes, by subcommand, with the code location, the count per normal run, and whether the outbox could replace it.

Citations verified at `4f9ec2c77`, 2026-11-14. None of the cited files changed through `78ce6dd70` (origin/main `037ba5a16`), the base this census was rebased onto.

Docs gate: `make docs-lint` (`go run ./cmd/docs-lint`) exits 0 on this file.

## Method and limits

- **Static.** Every count is derived from the formula text and the code paths. No seat was run and no sandbox was created, so no count is measured.
- **Normal run:** one seat, one pass through the eight formula steps, a code task that submits with `gt done --target main`, no escalation, no handoff, no compaction, one user prompt turn. Hook calls repeat per event, so they are counted per event.
- **Count notation:** `1` is one call on the path the formula directs. `0 or 1` and `0 to n` mean a condition decides it. Counts for the internals of `gt done` and `gt mol step done` are the call sites those commands reach on their linear path; branch-by-branch counts need a seat log, which this host does not have.
- **Line numbers** are checked at `4f9ec2c77`. They drift; re-grep the symbol before you rely on one.
- **Not counted:** `git` calls, and `bd`/`gt` calls made only by tests.
- **Formula source:** `internal/formula/formulas/mol-polecat-work.formula.toml`, cited as `formula:N`.

## Answers to the open facts

**1. Where TASK.md and the outbox go.** Put both under the sandbox workdir archive path, in one directory excluded through `.git/info/exclude`. Keep them out of the worktree root, because the formula's commit steps run `git add -A` (`formula:190`, `formula:367`). Two cautions:
- Git reads one `info/exclude` for every worktree of the repository (`internal/git/git.go:163`). The host should write the exclude line before the seat starts. A seat should not edit that file, because the edit reaches every worktree.
- Recommendation: a dedicated directory name, written by the host and never by the seat, so the exclude line cannot hide real work.

**2. Is `/sandbox/gt/.runtime` writable.** In this session, yes: `test -w` succeeds, the directory is `drwxr-xr-x polecat:polecat`, and the session runs as `polecat`. I did not write to it, and the seat's mount may differ from this session's view. The check to run in a real seat: `test -w /sandbox/gt/.runtime && echo writable`.

**3. Which calls need live reads.** See [Calls the outbox cannot replace](#calls-the-outbox-cannot-replace).

## Corrections to the starting facts

- **"No formula step reads live bead state" does not hold.** The formula reads live bead state at six lines: the assignment read and connectivity probe (`formula:81`, `formula:83`), the notes check (`formula:408`), and the acceptance reads (`formula:628`, `formula:633`, `formula:644`). A local `steps.json` can carry step status. It cannot carry the acceptance state these lines read.
- **"gt-wrap never runs inside a sandbox."** Not contradicted by the code: `cmd/gt-wrap` is a separate binary, and no `internal/cmd` path calls it. The seat formula calls `gt` directly. A real seat run is still the only proof.
- **The sandbox session end reads the agent bead live.** `internal/cmd/tap_polecat_stop_sandbox.go:112` calls `beads.GetAgentBead`, which runs `bd show`. The stop hook does this from inside the seat.
- **The formula text is stale on step beads.** `formula:20-22` says there are no separate step beads to close. `gt prime` tells the seat to finish each step with `gt mol step done` (`internal/cmd/prime_payload.go:304`), and this session's molecule lists step beads. The design should treat step beads as real.

## Census by call class

Replacement key: **yes** = the effect is a write the host can apply at seat end, or a read answered by TASK.md; **local** = answered by a file in the seat; **no** = needs a live read or a live write during the run.

### A. Calls the formula directs the polecat to make

| Call (as issued) | Code path | bd call behind it (read / write) | Per normal run | Outbox replaces? |
|---|---|---|---|---|
| `gt prime` (`formula:75`) | `internal/cmd/prime.go:1108`, `:1111`; `prime.go:909`, `:922` → `beads.go:2114`; `internal/cmd/hooked_work.go:55-56` → `beads.go:2685` | read: `show` agent bead, `show` hook bead, `list` assignee, `kv list` | 1 explicit, plus 1 SessionStart hook run (section B), so 2 per run; up to 4 bd reads per invocation | yes for the assignment and the checklist (TASK.md). `kv list`: drop it or pre-render it into TASK.md |
| `gt hook` (`formula:80`, no arguments) | `internal/cmd/hook.go:187` → `internal/cmd/molecule_status.go:356`, `:363` → `internal/cmd/hooked_work.go:55-56` | read: `show` agent bead, `list` assignee | 1 | yes (TASK.md) |
| `bd show {{issue}}` (`formula:81`) | direct | read: `show` | 1 | yes (TASK.md) |
| `bd list --limit 1` probe (`formula:83`) | direct | read: `list` | 0 on success; up to 3 on connectivity failure | no need: under no-bead-access the probe has nothing to test. Drop it and say so in the formula |
| `gt mail inbox` (`formula:91`) | `internal/cmd/mail_inbox.go:121` → `internal/mail/mailbox.go:126`; label at `internal/mail/router.go:296` | read: `list` (label `gt:message`) | 1 | **no**: inbound mail arrives during the run |
| `gt escalate` (`formula:61`, `:63`, `:85`, `:127`, `:208`, `:253`, `:337`) | `internal/cmd/escalate_impl.go:393`, `:427`, `:493`, `:522`, `:557`, `:671`, `:836` | read: `show`, `list`; write: create/update | 0; each escalation makes reads and a write | **no**: the operator needs the signal while the run is live |
| `gt bead note` (`formula:266`, `:268`, `:279`) | `internal/cmd/bead.go:483-492` (`AppendNotes` at `:487`) → `internal/beads/beads_release.go:132` | write: `update --append-notes` | 0 to n; a code task requires none | yes |
| `gt bead comment` (`formula:288`) | `internal/cmd/bead.go:618` → `internal/beads/beads.go:4282` | write: `comments add` | 0 (review-only beads only) | yes |
| `gt bead update --acceptance` (`formula:631`) | `internal/cmd/bead.go:494`, `:507` | write: one `update`; **no read and no if-acceptance guard** | 1 | yes, with a caveat: the write replaces the whole block, so a box another writer added after TASK.md was made is overwritten unless the host re-reads before it applies the outbox. The guarded read-then-write is `gt bead tick` (`bead.go:528-552`), which the formula does not use |
| `gt bead update --title` (`formula:641`) | `internal/cmd/bead.go:494`, `:507` | write: `update` | 0 or 1 (only when the title is disproved) | yes |
| `bd show {{issue}}` notes and acceptance reads (`formula:408`, `:628`, `:633`, `:644`) | direct | read: `show` | 408: 0 (report-only: 1); 628: 1; 633: 1; 644: 0 or 1 | yes: the seat's own copy comes from TASK.md; the host re-reads at seat end |
| `gt done --finding` (`formula:326`) | none in the seat: the finding is an argument to the submit call, and the town files the Found bead | write, via `gt done` | 0 to 5 values, all in the one submit | already outbox-shaped |
| `gt done --target main`, submit (`formula:668`) | reads: `internal/done/done.go:732`, `:1796`, `:2223`, `:2323`, `:2331`; `internal/cmd/done_agent_state.go:34`, `:265`, `:299`, `:330`; `internal/done/done_deferred_release.go:51`; `internal/done/done_rejection_unchanged.go:284`; `internal/done/source_validation.go:55`. Writes: `internal/done/done.go:1786`, `:2030`; `internal/cmd/done_agent_state.go:47`, `:458` | reads: `show`, `list`; writes: `update`, `close` | 1 invocation; its bd calls are the call sites in the code-path column (branch not traced) | writes: yes. Reads that decide whether `gt done` may finish: **no** (see below) |
| `gt handoff` (`formula:62`) | `internal/cmd/handoff.go:1804`, `:1813`, `:197` | runs `gt hook`, `gt mail inbox`, `gt done --status DEFERRED` as subprocesses | 0; each handoff runs all three | as the rows above |
| `gt done --status DEFERRED` (`formula:396`, `:671`) | `internal/cmd/molecule_step.go:506`; `internal/cmd/handoff.go:197` | write through `gt done` | 0 on a code run (exclusive with `--target`); 1 on a report-only run | yes |
| `gt mol step done <id>` (emitted by `gt prime`, `internal/cmd/prime_payload.go:304`) | `internal/cmd/molecule_step.go:96`, `:224-231`, `:275`, `:302`, `:368`, `:416`, `:499`, `:526`, `:536` | read: `show` step, `list` children, ready-steps query; write: `update` (`in_progress`, assign next), `close` | 7, one per step 1 to 7 (about 6 bd calls per invocation on the linear path); step 8 is closed by `gt done` or a final `gt mol step done`: not traced | step status: **local** (`steps.json`). Next-step text: yes (TASK.md carries the checklist). Close and assign writes: yes (outbox) |

**What `gt done` needs before it can finish.** The reads at `done_rejection_unchanged.go:284` (the rejection bead's current state) and the acceptance check the submit path runs decide whether `gt done` may finish. The outbox cannot answer them from a file the host wrote at spawn, because the bead can change during the run. These reads need a live answer or a host round trip before the seat submits.

**A host apply must not drop errors.** `internal/done/done.go:2030` ignores its `bd.Update` error (`_ =`). A host-side apply of the outbox copied from that line would report success on a failed write.

### B. Calls fired by hooks

Hook wiring: `internal/hooks/config.go`. Polecat overrides start at `:386`; base hooks at `:915`; crew-only entries at `:482-498`.

| Hook (event) | Code path | bd call behind it | Per normal run | Outbox replaces? |
|---|---|---|---|---|
| `gt prime --hook` (SessionStart, `config.go:988-994`; PreCompact, `:999-1005`) | `internal/cmd/prime.go` | as row A `gt prime` | 1 per session start; 0 for PreCompact unless the context compacts | as A `gt prime` |
| `gt mail check --inject` (UserPromptSubmit, `config.go:1010-1016`) | `internal/cmd/mail.go:269` → `internal/cmd/mail_check.go:17` → `mailbox.List` (`internal/mail/mailbox.go:126`) | read: `list` (label `gt:message`) | 1 per user prompt turn (at least 1). `prime.go:928` runs the same command as a subprocess from `gt prime` | **no** |
| `gt tap polecat-stop-check` (Stop, `config.go:398-404`, polecat override) | `internal/cmd/tap_polecat_stop.go:123` → `polecatStopSandboxEnd` (`:87`) → `endSandboxSessionIfDone` (`internal/cmd/tap_polecat_stop_sandbox.go:51`) → `readAgentBeadFields` (`:112`) | read: `show` of the agent bead via `beads.GetAgentBead` (`internal/beads/client_helpers.go:84`) | 1 per Stop event (at least 1) | yes: the host can read its own agent bead |
| Stop-time auto `gt done` (same hook) | `internal/cmd/tap_polecat_stop.go:190-195`; suites at `:488` | as A `gt done` | 0 on a normal run (only when the seat did not submit) | writes: yes. The gate runs in the seat either way |
| `gt tap nudge-drain` (PostToolUse, `config.go:387-393`, polecat override) | `internal/cmd/tap_nudge_drain.go:54-75` → `internal/cmd/nudge_drain.go:30` → `nudge.Drain` | none: the drain reads the local nudge queue. `internal/nudge/deliver/town.go:70` uses `bd`, but that is the sending side. `nudge.Drain` itself was not read | 1 per tool call; 0 bd calls | not applicable |
| `gt tap guard bd-close-invariant` (PreToolUse, `config.go:968`) | `internal/cmd/tap_guard_bd_close_invariant.go:338` (`beads.New(cwd).Show`); the bd-token filter at `:662-683` | `show` of the branch's bead, only when the command is `bd close` | 1 per tool call; 0 bd calls (no `bd close` in the formula) | moot if the seat has no `bd` binary; the design should decide |
| `gt tap guard bd-create-store` (PreToolUse, `config.go:975`) | `internal/cmd/tap_guard_bd_create_store.go:640` (path resolution, no `bd` process) | none | 1 per tool call; 0 bd calls | not applicable |
| `gt tap guard` for polecat-paths (`config.go:414`, `:427`), question-tool (`:447`), permission-request (`:465`, `:472`), pr-workflow (`:956`), dangerous-command (`:960`), container-suite (`:964`), sandbox-secrets (`:983`) | `internal/cmd/tap_guard*.go` | none | 1 per matching tool call; 0 bd calls | **no**: per-tool-call policy that must run in the seat |
| `gt tap guard script-files` (`config.go:431`, polecat; `:488`, crew-only) | `internal/cmd/tap_guard*.go` | none | 1 per matching tool call; 0 bd calls | **no** |
| `gt handoff --cycle --reason compaction` (`config.go:498`, crew-only) | not a polecat call | none | 0 | not applicable |

### C. Host-side at seat end (not seat calls; listed for the split)

- **Agent bead and session end.** The host reads the agent bead (`tap_polecat_stop_sandbox.go:112`) and kills the seat's session after `sandboxEndDelay`. Both are host-side work the seat does not need to perform.
- **Recovery.** `internal/cmd/sandbox_seat_recovery.go` judges a gone seat from git state and the remote branch. Its environment struct reads no bead; I did not trace every helper it calls.
- **Spec dispatch.** `internal/specdispatch/dispatch.go` has no direct `bd` or `gt` exec in the file. Its bead reads go through `internal/beads`; I did not trace them line by line.

## Summary by subcommand

Per-run counts are in the rows above. Totals per subcommand are not summed here, because the `gt done` and `gt mol step done` internals are not branch-traced.

| bd subcommand | Call sites from | Outbox replaces? |
|---|---|---|
| `bd show` | formula steps (`:81`, `:408`, `:628`, `:633`, `:644`); `gt prime`; `gt hook`; `gt mol step done`; `gt done`; `gt escalate`; stop hook; close-invariant guard; `gt bead` verbs that show before writing (`bead.go:530`, `:570`, `:592`, `:686`) | yes for the seat's own copies; **no** for reads `gt done` and `gt escalate` need live |
| `bd list` | `gt prime`; `gt hook`; `gt mail inbox` and `gt mail check --inject`; `gt escalate`; `gt mol step done`; `gt done` | `gt prime` and `gt hook`: yes; mail: **no**; `gt mol step done` children: **local** |
| `bd update` | `gt bead note` (`--append-notes`), `gt bead update` (`--acceptance`, `--title`), `gt bead claim`/`reset` (`bead.go:566`, `:591`), `gt mol step done`, `gt done`, `gt done --status DEFERRED` | yes (outbox writes) |
| `bd close` | `gt mol step done`; `gt done` (hooked bead, `done_agent_state.go:458`) | yes (outbox writes) |
| `bd comments add` | `gt bead comment` | yes |
| `bd kv list` | `gt prime` memory injection | yes: drop or pre-render |

The `gt bead claim` and `gt bead reset` verbs appear in the CLI (`internal/cmd/bead.go:566`, `:591`) but not in the formula. A seat that runs them is outside this census until the formula or hooks name them.

## Calls the outbox cannot replace

The outbox carries writes from the seat to the host, and TASK.md carries reads from the host to the seat at spawn. Anything that needs an answer during the run, or that reaches the seat during the run, needs a live channel:

1. **Inbound mail**: `gt mail inbox` (`formula:91`) and `gt mail check --inject` on every user prompt (`internal/cmd/mail.go:269`, `config.go:1016`).
2. **Escalation**: `gt escalate` (`escalate_impl.go`, see the rows above). The operator must see it while the run is live, so the write goes out at once, not at seat end.
3. **Submit-gate reads**: the rejection-unchanged read (`internal/done/done_rejection_unchanged.go:284`) and the acceptance checks inside `gt done`. `gt done` cannot finish until they answer, so the host must answer them before the seat submits, not at seat end.
4. **Liveness probe**: `bd list --limit 1` (`formula:83`). Under no-bead-access it has no target. Drop it rather than replace it.
5. **Per-tool-call guards**: the `gt tap guard` family (B). They must run in the seat. They make no `bd` call except bd-close-invariant, which is moot without `bd` in the seat.
