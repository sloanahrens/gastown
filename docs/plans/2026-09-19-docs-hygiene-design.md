> Status: historical (2026-09). Superseded by: gt-nj23 (epic). Not maintained.

# Docs and comments hygiene: one standard, a gate, and a weekly audit

Date: 2026-09-19. Epic: gt-nj23. Decisions were grilled and approved by Sloan on 2026-09-19 in the operator session; this document is the record.

## Destination

Every document an agent loads and every comment a reader meets in this repo is accurate and short. New rot is caught at the merge gate. Existing rot is paid down by a weekly audit that touches a bounded slice, submits one merge request, and rides the same review path as any other change. One file states the standard; everything else points at it.

## Baseline

| Measure | Value |
|---|---|
| Markdown files under docs/ | 73, about 154k words |
| Markdown lines added and removed, last 40 commits on main | +536 / -4 |
| Core reference docs (reference.md, overview.md, glossary.md) last edited | July 2026, by upstream |
| Lint that looks at comments | `misspell` only |
| om rubric criteria about docs or comments | none |
| Prior beads on the topic | gt-54i only |
| Agent-facing files over 1,500 words | 16, from 1,665 (mol-prd-review) to 28,912 (towers-of-hanoi-10) |
| Dead relative markdown links, rough count | 0 of 113 |
| `make` targets cited in docs that do not exist | 1 (`make test-changed`) |

om reviews diffs. It can catch rot a change introduces or leaves behind; it cannot find the drift already on main. The audit is the only mechanism that reads what nobody changed.

## Terms

- **Agent-facing doc**: a file an agent loads or is pointed at: AGENTS.md, `internal/templates/polecat-CLAUDE.md` (the source of every polecat's CLAUDE.md), docs/HOOKS.md, `plugins/*/plugin.md`, and the description blocks of `internal/formula/formulas/*.toml`. 63 files. The rig's own CLAUDE.md is gitignored and regenerated per clone, so it is not a doc this repo owns.
- **Reference doc**: a file under docs/ written for humans that is not historical. About 50 files.
- **Historical doc**: every file under docs/plans and docs/research, plus any file under docs/design whose first non-blank line is a `> Status:` header. About 20 files. Dated, not maintained, excluded from accuracy audits. docs/design is mixed: architecture.md, mail-protocol.md, escalation.md and their kin describe systems that exist on main and are cited from live docs, so they are reference; the proposal-shaped files (witness-at-team-lead.md, sandboxed-polecat-execution.md, agent-api-inventory.md, and the like) are historical. The bootstrap MR classifies each docs/design file by one rule: a doc that describes something on main today is reference; a doc that proposes, plans, or surveys is historical and gets the header. The header is then the classifier the lint script reads, so the tier list is never a second copy.
- **Slice**: the set of files one audit run may edit, chosen by the lint script, never by the polecat.
- **Run bead**: the gastown issue the scheduler creates for one audit run. Its open state is the double-dispatch guard; its comment carries the run's findings.
- Leading words borrowed from the writing-for-agents skill and used unchanged in the standard: **pointer**, **cache**, **no-op**, **sediment**, **sprawl**, **single source of truth**, **completion criterion**, **negation**.

## Decisions

Settled by grilling. Each is a constraint on the design below.

1. Scope: accuracy on agent-facing docs, reference docs, Go doc comments, and inline comments. Brevity on agent-facing docs and comments. Historical docs get a status header and are otherwise left alone; the audit may propose deleting one whose described work is verifiably merged.
2. Rigs: per-rig opt-in. gastown first. Other rigs copy the standard and enable the entries when they opt in.
3. Both mechanisms, rubric first: an om criterion catches new rot at the gate; a weekly audit pays down the backlog.
4. One source of truth for the standard, in the repo, reached by three pointers.
5. No upstream-mergeability constraint on docs. Pre-fork text is fully editable.
6. The deterministic checks run inside the existing lint gate, not in a new patrol.
7. The audit is a polecat on `deepseek-pro`, dispatched by a generic scheduled-sling patrol with a bead per run.
8. Measurement is the run bead comment and the om receipts. Nothing else is built.

## Components

### 1. The standard: docs/writing-for-agents.md

One file of flat, numbered rules. Each rule is one checkable line, followed by at most one short paragraph saying why. Rules are grouped under the leading words above so the auditor and the reviewer share the vocabulary. The rules:

- **Pointer**: a line that names a document states what it is and the condition for reading it, with the trigger word first.
- **Single source of truth**: each meaning lives in one place. A second copy is deleted and replaced by a pointer.
- **Cache**: a doc or comment that restates `--help` output, a config file, the Makefile, or the directory layout is a cache of a cheap lookup and is deleted. Cache only what a lookup cannot find: the convention, the reason, the gotcha.
- **No-op**: a sentence the agent would already obey by default is deleted whole.
- **Sediment**: a line that describes behaviour that no longer exists is deleted, not amended.
- **Sprawl**: an agent-facing file over 1,500 words is split by branch or its reference is pushed behind a pointer. The audit applies this to every file; the lint gate enforces a looser 2,000 on the always-loaded files only in v1 (component 3).
- **Completion criterion**: every step in a formula or directive ends with a condition an agent can check.
- **Negation**: state the target behaviour; a prohibition stands only as a guardrail with the positive form beside it.
- **Comments say why**: a comment states what the code visibly does only when the why is in it too.
- **Contract in one sentence**: a doc comment on an exported identifier states the contract, not the implementation.
- **History becomes a citation**: narrative history in a comment collapses to one bead-id citation. One citation per comment; the citation replaces the story, it never accompanies it.
- **Status header**: a historical doc starts with `> Status: historical (<month>). <Superseded by | Merged in | Abandoned>: <ref>. Not maintained.`
- **The test**: a line that changes nothing an agent or reader does is deleted.

The tier definitions live in prose in the standard, and the standard names `scripts/docs-lint.sh --list <tier>` as the authoritative list, so the list exists once.

Three pointers reach it:

- om: `"context_file": "docs/writing-for-agents.md"` in `.om.json`. The file is injected into every review prompt. Guaranteed, at about 1.5k tokens per review.
- The audit formula: its audit step opens by reading the file and ends on "every rule applied to every file in the slice".
- Polecats: one line in `internal/templates/polecat-CLAUDE.md`, the template gt renders into every polecat's CLAUDE.md: "Before writing or editing any comment, directive, formula, plugin.md, or doc: if the repo has docs/writing-for-agents.md, read it." The condition makes the pointer safe for every rig and makes opt-in equal to the file's presence. The rig CLAUDE.md cannot carry it: it is gitignored and regenerated per clone. A rig role directive could, but it lives in town config outside version control, and the template ships with the code.

### 2. The om criterion

Add to the gastown `.om.json` rubric:

```json
{"name": "docs-and-comments", "weight": 2,
 "guidance": "A change that alters behaviour updates every doc and comment that describes it. New or edited docs and comments follow docs/writing-for-agents.md (in the repository context). A stale or contradicted line in an agent-facing file (AGENTS.md, the polecat CLAUDE.md template, a directive, a formula description, a plugin.md, docs/HOOKS.md) is major; elsewhere minor. This criterion moves the score; it does not block alone."}
```

and `"context_file": "docs/writing-for-agents.md"` at the top level.

Weight 2 matches `tests`. Weight 1 could not move a verdict on a 3/3/2/2/3 rubric; weight 3 would let a comment nit reject a correct fix.

Two landing hazards, both procedural:

- om treats a missing `context_file` as an execution error, and the refinery reviews each MR in a checkout of that MR's branch. A branch cut before the standard exists fails every review with exit 2 until rebased. So the standard lands in its own MR first, and the `.om.json` change follows once in-flight branches have rebased past it.
- The rubric sha is pinned in the harness manifest. The moment the `.om.json` change merges, every gastown gate fails `version_mismatch` until the operator runs `deploy.sh --rig gastown` to re-stamp. The re-stamp follows the merge immediately.

### 3. make docs-lint

`scripts/docs-lint.sh`, bash, following the repo's script-plus-`_test.sh` pattern. It runs from the repo root, prints one line per finding as `path:line: rule: message`, and exits 1 on any finding.

Checks:

| Rule | Applies to | Finding |
|---|---|---|
| `dead-link` | agent-facing, reference, historical | a relative markdown link that does not resolve from the file's directory (URLs and pure anchors are skipped) |
| `dead-make-target` | agent-facing, reference | a backtick-quoted `make <target>` whose target is not defined in the Makefile (prose such as "make decisions" is not matched) |
| `status-header` | docs/plans, docs/research | the first non-blank line does not match `> Status: ` (docs/design files are historical only when they carry the header, so the header cannot be required there) |
| `word-ceiling` | AGENTS.md, polecat-CLAUDE.md, docs/HOOKS.md, plugin.md | more than 2,000 words |

The grilled ceiling was 1,500 words across the whole agent-facing tier. The baseline shows 16 files over it, 14 of them formulas, the largest patrol formulas at 4,800 to 8,600 words and the towers-of-hanoi demos far beyond. A gate that is red on day one is not a gate. So in v1 the ceiling covers the always-loaded files, set at 2,000 so the two largest (stuck-agent-dog plugin.md at 1,960, HOOKS.md at 1,870) pass with little room, and formulas are reported by `--words` but not gated. Formula sprawl is a finding under the standard's sprawl rule, which the audit applies as it cycles through them. A follow-up bead lowers the ceiling to 1,500 and adds formulas to it once the audit has visited every formula, and that bead is filed under the epic now so the deviation is recorded, not forgotten.

Modes for the audit:

- `--list <agent-facing|reference|historical|go>` prints the tier's files, one per line. The tier globs live here and nowhere else; historical is docs/plans, docs/research, and any docs/design file whose first non-blank line starts with `> Status:`.
- `--slice <docs> <go>` prints the `<docs>` agent-facing or reference files and the `<go>` non-test Go files least recently modified, by the date of the last commit touching each file. Ties break by path. Deterministic, so the polecat cannot choose its own slice.
- `--words` prints the total word count across the agent-facing tier.

Wiring: the `lint` target gains `docs-lint` as a dependency, so the rig's `lint_command` (`make lint`) and main_branch_test pick it up unchanged. Every MR gate and every main-branch check runs it.

Bootstrap: the introducing MR must leave main green, so it also adds the status header to every docs/plans and docs/research file and to the proposal-shaped docs/design files and fixes the `dead-link` and `dead-make-target` findings the first run reports. The rough baseline count is zero dead links and one dead make target, so the bulk of that MR is the headers and the docs/design classification. A finding that needs a judgement call is fixed by hand in that MR rather than allowlisted; there is no ignore file.

### 4. The audit formula: mol-doc-audit

Ships in `internal/formula/formulas/mol-doc-audit.formula.toml`. Same step skeleton as mol-polecat-work (load-context, branch-setup, audit, commit-changes, self-review, build-check, pre-verify, submit-and-exit) so crash-resume, pre-verify, and `gt done` are unchanged. Variables: `slice_docs` (default 8), `slice_go` (default 4).

The audit step:

1. Run `scripts/docs-lint.sh --slice {{slice_docs}} {{slice_go}}` and record the slice in the run bead's notes.
2. Read docs/writing-for-agents.md.
3. For every file in the slice, apply every rule. Edit in place. For a Go file, the scope is its comments only; code is not touched.
4. A historical doc in the slice is checked for its status header only. If the run finds its described work merged (a commit or bead it names is closed and on main), it records a deletion proposal with the evidence. It does not delete.
5. Each commit message names the rule ids applied, one commit per file or per rule group.

Completion criterion: every rule applied to every file in the slice, and the run bead comment posted.

Self-check before submit: `git diff --name-only origin/main...HEAD` is a subset of the slice. A file outside the slice is reverted, and the fact is recorded in the run bead comment. Then rebase onto current main and run the gates as pre-verify does today.

The run bead comment, posted before `gt done`, has four parts: files audited; findings per rule id, as counts; `docs-lint.sh --words` before and after; proposals the run could not act on (deletions, out-of-slice rot it noticed, rules that need sharpening).

The MR rides the normal refinery and om gate. om reviews it under the same criterion the audit worked to, with the standard in its context.

Pace at the defaults: the 97 doc files cycle in about twelve weeks; Go source is a slow sweep. That is acceptable because the rubric is the fast path for anything that changes, and a file nobody touched rots only when something it describes changed elsewhere, which the rubric's "updates every doc that describes it" clause is meant to catch at the source.

### 5. The scheduled_slings daemon patrol

A new opt-in patrol in `PatrolsConfig`, disabled when absent:

```json
"scheduled_slings": {
  "enabled": true,
  "entries": [
    {"name": "doc-audit", "rig": "gastown", "formula": "mol-doc-audit",
     "agent": "deepseek-pro", "interval": "168h", "priority": 3}
  ]
}
```

Each daemon tick evaluates every entry with a pure decision function `decideScheduledSling(beads, interval, now) action`, where `beads` is the rig's issues labelled `scheduled:<name>` with their status and creation time:

- any open bead: `skip` (a run is in flight, or its MR is waiting in the queue)
- newest bead created less than `interval` ago: `skip`
- otherwise: `dispatch`

On `dispatch`, the daemon creates a task bead in the rig titled `<name> <YYYY-MM-DD>` with label `scheduled:<name>` and the entry's priority, then runs `gt sling <bead> <rig> --formula <formula> --agent <agent> --actor daemon/scheduled:<name>`. The formula's slice variables come from the entry's optional `vars` map. The dispatch runs on its own goroutine, as main_branch_test does, so a slow sling never freezes the heartbeat.

Failure handling: a failed bead create or sling is logged with the entry name and retried on the next tick. After three consecutive failures for one entry the daemon escalates once with the last error and keeps retrying. A missed weekly audit is not urgent; a silently broken one is.

No state file. Last-run time is the newest bead's creation timestamp; the open bead is the guard. Both survive daemon restarts and Dolt flattens as well as any bead does.

Fallback if the patrol slips: a launchd agent running the same bead-create-and-sling as a shell script once a week. Same formula, different clock, no guard.

### 6. Testing

- docs-lint: `scripts/docs-lint_test.sh` runs the script against a fixture tree under `scripts/testdata/docs-lint/` containing one violation per rule and one clean file per tier, and asserts the exact finding lines and exit codes. `--slice` is tested against a fixture repo built in a temp dir with commits at known dates.
- Decision function: table-driven Go tests over open, recent, due, and empty bead sets, and over a mix of open and closed.
- Dispatch: the bead-create and sling calls go through an injectable runner, the same pattern main_branch_test uses, so tests assert the argv without running gt.
- Config: a test that a daemon.json without the block leaves the patrol disabled and that a malformed interval is reported, not silently zero.
- Formula: the existing shipped-formula parse test covers the new file.
- End to end: one hand-run sling of a doc-audit bead with `--agent deepseek-pro` before the patrol is armed. The MR it produces is read by the operator before anything is automated.

## Rollout

Four merge requests, each usable alone, in this order.

1. The standard, the polecat template pointer, docs-lint with its tests, the Makefile wiring, the status headers on historical docs, the docs/design classification, and the link and make-target fixes the first run reports. Main is green on merge. This may land as several MRs, but the Makefile wiring that makes docs-lint part of `make lint` lands in the same MR as the fixes that make it green.
2. The `.om.json` criterion and `context_file`, filed only after in-flight branches have rebased past MR 1. The operator re-stamps the gastown manifest immediately after merge.
3. The formula, plus one hand-run sling. The operator reads the resulting MR and the run bead comment. If the om gate rejects two audit MRs in a row, the formula or the rule text is fixed before step 4.
4. The patrol, its tests, and the daemon.json entry, armed only after the hand run's MR merged cleanly.

## Tracking

Epic gt-nj23 in the gastown rig. Each rollout step becomes one bead under it, dependencies wired in that order. Runs of the audit are found with `bd list --label scheduled:doc-audit`.

## Risks

- **Gate gap on landing**: covered in component 2. The two hazards are procedural and are written into the beads for MR 1 and MR 2.
- **The audit rewrites more than it should**: bounded by the slice, enforced by the self-check, and reviewed by om under the same standard. deepseek-pro runs it, not the rig's default polecat model, because a doc auditor that improvises is worse than none.
- **Bootstrap debt is larger than the rough count**: the baseline link check was a shell approximation. MR 1's bead records the script's own count before work starts; if it is large the fixes are split into a second bead, but the bar is still that main is green when the lint target is wired.
- **Standard is wrong in places**: the run bead comment has a slot for "rules that need sharpening". The standard is a doc under its own rules and gets audited like any other file.
- **Formulas stay long**: they are outside the v1 ceiling, and gt prime output is already truncated at the 10k hook-output limit, so a long formula costs real delivery. The audit's sprawl rule is the only pressure on them until the follow-up bead adds them to the gate. If the audit's first two formula visits produce no shrinkage, the ceiling bead is pulled forward.
