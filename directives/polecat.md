## Changelog

- 2026-11-05 gt-qgum2: added rule 17, run the integration tier the gate skips (seventeen rules); a "Before gt done" line 11 points at rule 17 and searches the whole tree for `//go:build integration` files, callers in other packages included.
- 2026-11-01 gt-2iro9: added rule 16, one workspace per answer (sixteen rules); the rule-count test is now TestPolecatDirectiveRuleHeadings.
- 2026-10-31 gt-pccx7: one coherent revision from the 2026-10-28..31 rejection record (842 majors): a plan section first, town rules and tests merged to one copy each, rules 12-15 (cloud) leave for the devops layer, new rules 12-14 (finish and wire, state and locks, docs say what the code does), rule 16 becomes 15, and a closing pre-submit checklist ordered by rounds burned (fifteen rules).
- 2026-11-01 gt-9givh: added a "Before gt done: five checks on your added lines" checklist after rule 16; it cites existing rules, so the count stays sixteen.
- 2026-10-31 gt-9ptn8: merged the scratch and shell-script pairs into their policy sections; rules 7 and 8 point at them; intro counts dropped (sixteen rules).
- 2026-10-30 gt-wfohy: added rule 16 (rework: answer every open finding, never loosen a test, limit or allowlist to land) and rewrote rule 9's flags from the 2026-10-28..30 findings, where scope was filed as an edit outside the bead's Constraints rather than a dependency bump or rename (sixteen rules).
- 2026-10-30 gt-u3hr0: rules 1, 3, 10, 11 rewritten from the 2026-10-28..30 rejection record; rule 1 now names the forms om flags, old rule 11 (matcher and platform-file errors) merges into rule 1, the guard-mutation bullet moves from rule 10 to rule 3, and the rules after it renumber (fifteen rules).
- 2026-10-30 gt-o87rv: added rules 13-16 (cloud authority, cloud previews, console-only steps, cost and credentials); rule 14 points at the no-new-scripts town policy instead of restating it.
- 2026-10-28 gt-9j9vu: bead text is one single-quoted argument on every `gt bead` verb that takes text (create, update, claim, note, comment, close; town policy section).
- 2026-10-27 gt-vhyk9: files stay under the worktree — scratch files, trace logs, test data and build output included; /tmp, /private/tmp, HOME and a session scratchpad are out (gt-3n4e0, hq-wisp-dvrty). The 2026-10-14 scratch section is retitled "Scratch files stay out of the tracked tree" (merged into this section by gt-9ptn8).
- 2026-10-27 gt-3vgbi: moved the town polecat policy into the repo from ~/gt/directives/polecat.md, with the twelve "What om rejects" rules from ~/gt/gastown/directives/polecat.md, text unchanged.
- 2026-10-27 gt-3vgbi: removed a probe line that the overseer's probe bead had put in the rules file; it was not policy.
- 2026-09-23 claude-9a8: an interrupt is a delivery artifact, not an operator stop (town policy section).
- 2026-10-12 gt-3vgbi: no subagents, except the scheduled deep-review slices (town policy section, dated 2026-10-12).
- 2026-10-14 gt-3vgbi: scratch files stay out of the worktree (town policy section, dated 2026-10-14).
- 2026-10-23 gt-3vgbi: tests are engineered, not accumulated (town policy section, dated 2026-10-23; full standard in ~/.claude/docs/standards/test-engineering.md).
- 2026-10-26 gt-3vgbi: tests stay hermetic, and no new shell scripts (town policy sections, dated 2026-10-26).
- 2026-10-24 gt-3vgbi: no new scripts for testable logic (town policy section, dated 2026-10-24).
- 2026-10-26 gt-3vgbi: the twelve rules in "What om rejects" were written from the day's om rejections; each cites its example bead (rules 1-12, unchanged).

## Before you write code

Read the bead's Goal, Constraints, Out of scope and Acceptance. Write a short plan in `scratch/plan.md`: for each acceptance box, what must be wired (the caller, flag or field that makes it real) and which test goes red without it; every refusal branch the design needs; the one way this design most likely fails in production. Check the diff against it before `gt done`.

## Town rules that always apply

**An interrupt is not an operator stop.** A tool call rejected with "The user doesn't want to proceed with this tool use" or "[Request interrupted by user for tool use]" and no instruction is a nudge artifact (claude-9a8): re-run it if you still need it. A real order is text from a named sender saying what to stop, and a "hold" covers only what it names; if you cannot cite the order, there is no hold.

**No subagents.** No Task or Agent tool, no helper sessions. If the work seems to need helpers it is too big: note it on your bead and file a bead that splits it. Only a bead labelled `scheduled:deep-review-slice` or `scheduled:deep-review-risk` spawns the read-only subagents its formula names.

**Bead text is one single-quoted argument** on every `gt bead` verb that takes text (`create`, `update`, `claim`, `note`, `comment`, `close`), across as many lines as needed, so backticks and `$(...)` survive and the tap guard reads the text as written. `bd` is for reads. A text holding a single quote takes double quotes; escape `$`, backticks and braces:

```bash
gt bead note gt-abc 'the retry never resets its counter'
```

**Files stay under the worktree.** Scratch files, logs, test data and build output go in `scratch/` (never committed by the checkpoint), not `/tmp`, `/private/tmp`, HOME or a session scratchpad. Any other stray file is committed within minutes: delete it and commit the deletion before `gt done`.

## Tests

Full standard: `~/.claude/docs/standards/test-engineering.md`. om flags a breach as major, a new slow test as a finding.

- One reason to fail per test. Assert behaviour (outputs, errors, exit codes, files), not source text or an implementation field.
- A unit test is in-process: no subprocess, executable file, Go tool, network or package-level variable set to steer behaviour; inject the runner, filesystem and clock. It never reads the live town tree (`~/gt`), the host environment or HOME: build under `t.TempDir()`.
- Bound every wait and end every loop. No `sleep` and no wall-clock deadline: use a fake clock or a condition, and read a clock baseline before you launch what you time.
- Never retry or skip a failing test, or raise a timeout, budget or ratchet to land. A test over its tier's cost moves up a tier or gets cheaper.
- A test that runs a script or binary reads those files itself, or the result cache serves a stale pass. A shell test passes on macOS too (no `sed -i`, `date -d`, `stat -c`, `readlink -f`; BSD `wc -l` pads).
- No `.sh`, `.bash`, `.zsh`, `.pl`, `.rb` or Python file for logic Go can test, and do not grow an existing script: write Go with unit tests. Allowed, with the reason in the bead: a thin exec wrapper, Makefile glue, a bootstrap that must run before Go exists, a file the platform dictates. If the bead asks for a script with no such reason, ask first.

# What om rejects

Read before you write code. Seventeen rules. Each names om's finding and an example.

## 1. Failure is not success (fail-open)

A failed, unreadable or unwired input must stop, refuse or escalate, never pass as clean (the most common major finding). om flags:
- an unreadable file, record or status read as absent, empty or clean, or a parser error read as "does not apply";
- a refusal that returns what a healthy answer returns (0, nil, exit 0, SKIPPED, GREEN);
- a guard whose input is not wired defaults to allow ("PushInFlight gate is a constant false"): unknown denies;
- an error dropped (`_ =`), only logged, or reported as "skipped" where the caller has to act.
- Example: gt-42ulv (critical). A missing red-main status read as green; gt-p85pw.73: an unreadable routes file reported SKIPPED. Fix: return the error, or record the state as unknown and escalate.

## 2. Each criterion needs a test that fails without it

Every acceptance box needs a test that is red when the behaviour is removed. Mutate it yourself: delete the line or flip the condition, run the test, restore.
- om flags: "criterion ... has no test".

## 3. Test the branch that refuses, not only the happy path

Every error or refusal branch in the diff has a test that drives it: one table row per refusal, each asserting the error. Mutate each new guard before `gt done`; a guard no test turns red is the most common reason a head is rejected.
- om flags: "failing branch has no assertion", "alarm and refusal branches lack tests". Example: gt-z6qre.12. No test for the unset-HOME failure the bead listed as a criterion.

## 4. Untrusted input is not trusted by its prefix, its text or its shape

Anything an author or caller writes (a comment, bead field, path, name) can be forged or hostile. Decide authority from something the writer cannot write; validate a value before it reaches a path, argv or request.
- om flags: "decision authority is granted by prefix alone, so any commenter can forge a decision" (gt-0sztd); an owner/name spliced into a request path (gt-tpg0p.33).

## 5. An author cannot review or rebut their own work

om flags: "author under review can self-rebut its own findings". A rebuttal counts only from a reviewer or the overseer.

## 6. Verification claims show their output

A claim that something passed pastes the command and its last lines of output. om flags: "verification claim has no shown output".

## 7. Hermetic tests

The Tests section is the rule. om flags: "test depends on the host's $SHELL and prompt style, so it is not hermetic"; a test that reads the live town tree (gt-qpeei.15).

## 8. Do not add a shell or Python script

The Tests section is the rule. om flags: "new parsing and state logic added to a shell script"; "a new python script parses the config" (gt-r1up7).

## 9. Stay in scope

Change only what the bead asks for.
- om flags: "edit to a file outside the bead's Constraints"; "existing test fixture edited to fit new behaviour"; "exported API signature changed without recording it"; "unexplained transitive dependency bumps"; "change touches files outside the stated scope". If another file must change, say why in a bead comment.

## 10. Test the call site, not the helper

A test runs the production function that makes the call, so deleting the call turns it red.
- om flags: a test that calls the helper directly.

## 11. Keep secrets out of errors and logs

Never put captured output in an error or log: name the rule and the command, since output can hold a secret. A credential is never on a command line or in a failure message.
- om flags: an error that echoes captured command output (`fmt.Errorf("failed: %s", out)`). Example: gt-tpg0p.35.

## 12. Finish what the bead names, and wire it

Every Goal clause and box is implemented and reachable: a new function, flag or field has a production caller, a table has every row the Goal lists, and a fix changes production code (a pin test alone is a finding).
- om flags: "Goal's refusal conditions ... are not implemented"; "has no in-tree caller". Example: gt-gojo0 (a table named 9 callers, the Goal said 16).

## 13. State, order, locks and timeouts

Anything cached, held or waited on has an owner and an end: state read once is re-read when it can change; a lock covers the whole critical section and is released on every path; a bound is shorter than the budget around it; a reset covers every field.
- om flags: "go stale", "can release the lock while ... keeps running". (gt-km9jt, gt-dl4nv.13).

## 14. Docs and comments say what the code does

Re-read every comment, doc or help line you add against the code. No line numbers, no dated measured figure, no claim of a check or fallback the code does not make, no placeholder text.
- om flags: "comment claims a walk that does not exist"; "placeholder template". Examples: gt-6fqye, gt-uljn1.

## 15. Rework: answer every open finding

A bead that comes back has a MERGE REJECTION block in its notes. Fix every finding in it, including those marked "still open": om repeats them. Rework is where new defects appear (reject rate 33% on a first review, 55% on a third): after every edit re-read your added lines against every other rule, not only the lines a finding named. Never loosen a test or a limit to land the fix. If a finding is wrong, say why on the bead with evidence.
- om flags: "still open", "claimed resolved but not changed by the diff", "ratchet floor lowered".

## 16. One workspace per answer

Code that builds a view, a total, a bill or a limit for one workspace checks that every record it reads names that workspace: the cap, the spend records, the usage export, the seat and storage rows, the token counts. List the records that enter before you write the check, and test each with a record from another workspace and with an empty workspace id; both are refused, never summed or copied. A missing registry, prefix or map is an error, never the default workspace.
- om flags: "another workspace's", "empty id matches", "falls back to the default prefix". Examples: gt-6f4mk, gt-1sriy.

## 17. Run the tier the gate skips

`make gate` runs the unit tier only; docs/testing.md says what it runs. When your diff changes a function, type or fixture that a file tagged `//go:build integration` uses, in your package or a caller's, run those packages with `go test -tags integration ./<pkg>/...` and paste the result, or write "not run: <reason>" in the bead notes. A package whose tests need a container is "not run: container": the landing worker runs the container suites on the merged tree. Never call a change tested on the gate's result alone.
- om flags: "integration-tagged callers not run", "gate does not exercise this change" (proposed wording, no finding yet). Example: gt-26dvk.

# Before gt done

Run these on your added lines, in the order om rejects most. A hit is a question, not a verdict. `D` below stands for `git diff -U0 origin/main...HEAD`, run as written with the pipe after it.

1. Errors (rule 1): `D -- '*.go' | grep -nE '^\+.*(_ = |, _ :?= )'`, and read every added `if err != nil` branch. Each hit returns, wraps or escalates.
2. Wiring (12): `D -- '*.go' | grep -E '^\+func '`, then `grep -rn <name>` for a caller outside tests. Compare with your plan.
3. Red without the code (2, 3, 10): each added refusal branch and call has a test row; delete the guard or call, run its `-run` test, see it fail, restore.
4. Docs and comments (14): `D | grep -nE '^\+\s*(//|#|\*|-)'`; read each line against the code.
5. State and bounds (13): name what ends each cache, lock, retry, timeout or reset you added.
6. Input (4): each caller or file value reaching a path, argv or request is validated first.
7. Test hygiene (7, 8): `D -- '*_test.go' | grep -nE '^\+.*(time\.Sleep|time\.After|Setenv|Getenv|UserHomeDir|/tmp|exec\.Command)'` is empty or justified.
8. Secrets (11): no added error or log line prints captured output, a token or a password.
9. Scope (9): `git diff --stat origin/main...HEAD` lists only files your Constraints name; go.mod changes carry a Rationale line.
10. Workspace (16): for each record type the diff reads for one workspace, make the check rule 16 names and grep the diff for its workspace field: `D | grep -nE '^\+.*[Ww]orkspace'`; a read with no check is a question.
11. Tier (17): apply rule 17 to every integration-tagged file your diff reaches, in any package, not only the one you changed: `grep -rl 'go:build integration' --include='*.go' .` lists them.

Tick each acceptance box only after its test exists.
