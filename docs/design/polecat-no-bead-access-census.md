# Polecat no-bead-access: census of seat-side bd and gt calls

Input to the no-bead-access design (approach A: TASK.md in, outbox file out, host applies the outbox at seat end; sandbox seats only). It lists every `bd` and `gt` call a sandbox seat makes, by subcommand, with the code location and whether the outbox could replace it.

## Method and limits

- **Static.** Counts are call sites read from the formula, the hook configuration and the Go code paths the seat runs. No seat was run and no sandbox was created for this measurement.
- **No per-run counts.** The bead asks for calls per completed seat run. This host has no seat command log: `/sandbox/gt/.runtime` holds only `heartbeats/`. Per-run numbers need a log from a real seat.
- **Line numbers** are at the branch base (`76f3f008`). They will drift; re-grep the symbol before you rely on one.
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

## Census by call class

Replacement key: **yes** = the effect is a write the host can apply at seat end, or a read answered by TASK.md; **local** = answered by a file in the seat; **no** = needs a live read or a live write during the run.

### A. Calls the formula directs the polecat to make

| Call (as issued) | bd subcommand behind it | Code path | Reads / writes | Outbox replaces? |
|---|---|---|---|---|
| `gt prime` (`formula:75`) | `bd show` agent bead; `bd show` hook bead; `bd list --json` (assignee); `bd kv list --json` | `internal/cmd/prime.go:1108`, `:1111`; `internal/cmd/hooked_work.go:55-56` → `internal/beads/beads.go:2685`; `internal/cmd/prime.go:909`, `:918-922` → `beads.go:2114` | read | yes for the assignment and the checklist (TASK.md). kv list: drop it or pre-render it into TASK.md. |
| `gt hook` (`formula:80`, no arguments) | `bd show` agent bead; `bd list` (assignee) | `internal/cmd/hook.go:187` → `runMoleculeStatus`, `internal/cmd/molecule_status.go:356`, `:363` → `internal/cmd/hooked_work.go:55-56` | read | yes (TASK.md) |
| `bd show {{issue}}` (`formula:81`) | `bd show` | direct | read | yes (TASK.md) |
| `bd list --limit 1` probe (`formula:83`) | `bd list` | direct | read | no need: under no-bead-access the probe has nothing to test. Drop it and say so in the formula. |
| `gt mail inbox` (`formula:91`) | `bd list` (mail is bead-backed, label `gt:message`) | `internal/cmd/mail_inbox.go:121` → `internal/mail/mailbox.go:126`; label at `internal/mail/router.go:296` | read | **no**: inbound mail arrives during the run |
| `gt escalate` (`formula:61`, `:63`, `:85`, `:127`, `:208`, `:253`, `:337`) | `bd show`, `bd list`, create/update of the escalation bead | `internal/cmd/escalate_impl.go:393`, `:427`, `:493`, `:522`, `:557`, `:671`, `:836` | read / write | **no**: the operator needs the signal while the run is live |
| `gt bead note` (`formula:266`, `:268`, `:279`) | `bd update --append-notes` | `internal/cmd/bead.go:483-493` → `internal/beads/beads_release.go:132` | write | yes |
| `gt bead comment` (`formula:288`) | `bd comments add` | `internal/cmd/bead.go:618` → `internal/beads/beads.go:4282` | write | yes |
| `gt bead update --acceptance` (`formula:631`) | `bd show` (compare), then `bd update` with an if-acceptance guard | `internal/cmd/bead.go:494`, `:528-552` (`:530` show, `:552` update) | read, then write | yes if the host applies the write and repeats the if-acceptance check against live state |
| `gt bead update --title` (`formula:641`) | `bd update` | `internal/cmd/bead.go:494` | write | yes |
| `bd show {{issue}}` acceptance and notes reads (`formula:408`, `:628`, `:633`, `:644`) | `bd show` | direct | read | yes: the seat's own copy comes from TASK.md; the host re-reads at seat end |
| `gt done --finding` (`formula:326`) | none in the seat: the town files the Found bead | the finding is already an outbox-shaped value | write | already outbox-shaped |
| `gt done`, submit (`formula:609-671`) | `bd show`, `bd update`, `bd list`, `bd close` (hooked bead) | reads: `internal/done/done.go:732`, `:1796`, `:2223`, `:2323`, `:2331`; `internal/cmd/done_agent_state.go:34`, `:265`, `:299`, `:330`; `internal/done/done_deferred_release.go:51`; `internal/done/done_rejection_unchanged.go:284`; `internal/done/source_validation.go:55`. Writes: `internal/done/done.go:1786`, `:2030`; `internal/cmd/done_agent_state.go:47`, `:458` | mixed | writes: yes. Reads that decide whether `gt done` may finish: **no** (see below). |
| `gt handoff` (`formula:62`) | `gt hook`, `gt mail inbox`, `gt done --status DEFERRED` as subprocesses | `internal/cmd/handoff.go:1804`, `:1813`, `:197` | mixed | as the rows above |
| `gt done --status DEFERRED` (`formula:396`, `:671`) | `bd` writes through `gt done` | `internal/cmd/molecule_step.go:506`; `internal/cmd/handoff.go:197` | write | yes |
| `gt mol step done <id>` (emitted by `gt prime`, `internal/cmd/prime_payload.go:304`) | `bd show` (step), `bd list` (children), ready-steps query, `bd update` (`in_progress`), `bd close`, `bd update` (assign next) | `internal/cmd/molecule_step.go:96`, `:224-231`, `:275`, `:302`, `:368`, `:416`, `:499`, `:526`, `:536` | read / write | step status: **local** (`steps.json`). Next-step text: yes (TASK.md carries the checklist). Close and assign writes: yes (outbox). |

**What `gt done` needs before it can finish.** The reads at `done_rejection_unchanged.go:284` (the rejection bead's current state) and the acceptance check the submit path runs decide whether `gt done` may finish. The outbox cannot answer them from a file the host wrote at spawn, because the bead can change during the run. These reads need a live answer or a host round trip before the seat submits.

### B. Calls fired by hooks

Hook wiring: `internal/hooks/config.go`. Polecat overrides start at `:386`; base hooks at `:915`; crew-only entries at `:482-498`.

| Hook call | Code path | bd call behind it | Outbox replaces? |
|---|---|---|---|
| `gt prime --hook` (SessionStart, PreCompact) — `config.go:994`, `:1005` | `internal/cmd/prime.go` (as row A `gt prime`) | as A `gt prime` | as A `gt prime` |
| `gt mail check --inject` (`config.go:1012-1016`) | `internal/cmd/prime.go:928` | as A `gt mail inbox` | **no** |
| `gt tap polecat-stop-check` (`config.go:404`, polecat override) | `internal/cmd/tap_polecat_stop.go:123` → `polecatStopSandboxEnd` (`:87`) → `endSandboxSessionIfDone` (`internal/cmd/tap_polecat_stop_sandbox.go:51`) → `readAgentBeadFields` (`:112`) | `bd show` of the agent bead via `beads.GetAgentBead` (`internal/beads/client_helpers.go:84`) | yes: the host can read its own agent bead |
| Stop-time auto `gt done` (same hook) | `internal/cmd/tap_polecat_stop.go:190-195`; suites at `:488` | as A `gt done` | writes: yes; the gate runs in the seat either way |
| `gt tap nudge-drain` (`config.go:393`, polecat override) | not traced | not traced | not traced |
| `gt tap guard bd-close-invariant` (`config.go:968`) | `internal/cmd/tap_guard_bd_close_invariant.go:336-338` (`beads.New(cwd).Show`); the guard self-filters to `bd` tokens at `:633-654` | `bd show` of the branch's bead, on a `bd close` only | **moot** if the seat has no `bd` binary; the design should decide |
| `gt tap guard bd-create-store` (`config.go:975`) | `internal/cmd/tap_guard_bd_create_store.go:640` (path resolution, no `bd` process) | none | not applicable |
| `gt tap guard` for pr-workflow, dangerous-command, container-suite, sandbox-secrets, polecat-paths, question-tool, permission-request (`config.go:414-472`, `:956-983`) | `internal/cmd/tap_guard*.go` | none | **no**: per-tool-call policy that must run in the seat |
| `gt tap guard script-files` (`config.go:431`, `:488`) | `:488` is crew-only (`config.go:482`) | none | **no** |
| `gt handoff --cycle --reason compaction` (`config.go:498`, crew-only) | not a polecat call | — | not applicable |

### C. Host-side at seat end (not seat calls; listed for the split)

- **Agent bead and session end.** The host reads the agent bead (`tap_polecat_stop_sandbox.go:112`) and kills the seat's session after `sandboxEndDelay`. Both are host-side work the seat does not need to perform.
- **Recovery.** `internal/cmd/sandbox_seat_recovery.go` judges a gone seat from git state and the remote branch. Its environment struct reads no bead; I did not trace every helper it calls.
- **Spec dispatch.** `internal/specdispatch/dispatch.go` has no direct `bd` or `gt` exec in the file. Its bead reads go through `internal/beads`; I did not trace them line by line.

## Summary by subcommand

Every call site above, grouped by the `bd` subcommand it runs. Counts would be per-run and need a seat log (see Method).

| bd subcommand | Call sites from | Outbox replaces? |
|---|---|---|
| `bd show` | formula steps (`:81`, `:408`, `:628`, `:633`, `:644`); `gt prime`; `gt mol step done`; `gt done`; `gt escalate`; stop hook; close-invariant guard; `gt bead` verbs that show before writing (`bead.go:530`, `:570`, `:592`, `:686`) | yes for the seat's own copies; **no** for reads `gt done` and `gt escalate` need live |
| `bd list` | `gt prime`; `gt hook`; `gt mail inbox` and `gt mail check --inject`; `gt escalate`; `gt mol step done`; `gt done` | `gt prime`/`gt hook`: yes; mail: **no**; `gt mol step done` children: **local** |
| `bd update` | `gt bead note` (`--append-notes`), `gt bead update`, `gt bead claim`/`reset` (`bead.go:566`, `:591`), `gt mol step done`, `gt done`, `gt done --status DEFERRED` | yes (outbox writes) |
| `bd close` | `gt mol step done`; `gt done` (hooked bead, `done_agent_state.go:458`) | yes (outbox writes) |
| `bd comments add` | `gt bead comment` | yes |
| `bd kv list` | `gt prime` memory injection | yes: drop or pre-render |

The `gt bead claim` and `gt bead reset` verbs appear in the CLI (`internal/cmd/bead.go:566`, `:591`) but not in the formula. A seat that runs them is outside this census until the formula or hooks name them.

## Calls the outbox cannot replace

The outbox carries writes from the seat to the host, and TASK.md carries reads from the host to the seat at spawn. Anything that needs an answer during the run, or that reaches the seat during the run, needs a live channel:

1. **Inbound mail**: `gt mail inbox` (`formula:91`) and `gt mail check --inject` (`internal/cmd/prime.go:928`, `config.go:1016`).
2. **Escalation**: `gt escalate` (`escalate_impl.go`, see row list above). The operator must see it while the run is live, so the write goes out at once, not at seat end.
3. **Submit-gate reads**: the rejection-unchanged read (`internal/done/done_rejection_unchanged.go:284`) and the acceptance checks inside `gt done`. `gt done` cannot finish until they answer, so the host must answer them before the seat submits, not at seat end.
4. **Liveness probe**: `bd list --limit 1` (`formula:83`). Under no-bead-access it has no target. Drop it rather than replace it.
5. **Per-tool-call guards**: the `gt tap guard` family (B). They must run in the seat. They make no `bd` call except bd-close-invariant, which is moot without `bd` in the seat.
