## Changelog

- 2026-10-30 gt-wfohy: added rule 16 (rework: answer every open finding, never loosen a test, limit or allowlist to land) and rewrote rule 9's flags from the 2026-10-28..30 findings, where scope was filed as an edit outside the bead's Constraints rather than a dependency bump or rename (sixteen rules).
- 2026-10-30 gt-u3hr0: rules 1, 3, 10, 11 rewritten from the 2026-10-28..30 rejection record; rule 1 now names the forms om flags, old rule 11 (matcher and platform-file errors) merges into rule 1, the guard-mutation bullet moves from rule 10 to rule 3, and the rules after it renumber (fifteen rules).
- 2026-10-30 gt-o87rv: added rules 13-16 (cloud authority, cloud previews, console-only steps, cost and credentials); rule 14 points at the no-new-scripts town policy instead of restating it.
- 2026-10-28 gt-9j9vu: bead text is one single-quoted argument on every `gt bead` verb that takes text (create, update, claim, note, comment, close; town policy section).
- 2026-10-27 gt-vhyk9: files stay under the worktree — scratch files, trace logs, test data and build output included; /tmp, /private/tmp, HOME and a session scratchpad are out (gt-3n4e0, hq-wisp-dvrty). The 2026-10-14 scratch section is retitled "Scratch files stay out of the tracked tree".
- 2026-10-27 gt-3vgbi: moved the town polecat policy into the repo from ~/gt/directives/polecat.md, with the twelve "What om rejects" rules from ~/gt/gastown/directives/polecat.md, text unchanged.
- 2026-10-27 gt-3vgbi: removed a probe line that the overseer's probe bead had put in the rules file; it was not policy.
- 2026-09-23 claude-9a8: an interrupt is a delivery artifact, not an operator stop (town policy section).
- 2026-10-12 gt-3vgbi: no subagents, except the scheduled deep-review slices (town policy section, dated 2026-10-12).
- 2026-10-14 gt-3vgbi: scratch files stay out of the worktree (town policy section, dated 2026-10-14).
- 2026-10-23 gt-3vgbi: tests are engineered, not accumulated (town policy section, dated 2026-10-23; full standard in ~/.claude/docs/standards/test-engineering.md).
- 2026-10-26 gt-3vgbi: tests stay hermetic, and no new shell scripts (town policy sections, dated 2026-10-26).
- 2026-10-24 gt-3vgbi: no new scripts for testable logic (town policy section, dated 2026-10-24).
- 2026-10-26 gt-3vgbi: the twelve rules in "What om rejects" were written from the day's om rejections; each cites its example bead (rules 1-12, unchanged).

## An interrupt is a delivery artifact, not an operator stop (town policy, 2026-09-23)

> **Town Policy — overrides formula instructions where they conflict.**

No human types into this pane. A tool call rejected with "The user doesn't
want to proceed with this tool use" or "[Request interrupted by user for tool
use]", with no instruction attached, is a nudge-delivery artifact (claude-9a8),
not an order to stop. Re-run the interrupted command if you still need it and
continue your loop.

Real orders arrive as text, from a named sender, and say what to stop. A
"hold" covers only what it names ("no new slings" stops slings), never your
patrol loop. Before replying "holding", cite the order: sender, time, and the
words that name your loop. If you cannot cite one, there is no hold: resume.

## No subagents (town policy, 2026-10-12)

> **Town Policy — overrides formula instructions where they conflict.**

Your work was sized for one agent. Do not spawn subagents: no Task or Agent tool, no
parallel helper sessions, no "reviewer" or "verifier" sub-sessions. Read, edit and test
in your own session. If the work seems to need helpers, it is too big: stop, say so in a
note on your bead and file a bead that splits the work.

Exception, and only this one: a bead labelled `scheduled:deep-review-slice` or
`scheduled:deep-review-risk` is a scheduled deep review. Its formula orchestrates read-only
reviewer and verifier subagents (at most the formula's caps), and it never edits, commits or
pushes. Spawn them as the formula says.

## Bead text is one single-quoted argument (town policy, 2026-10-28)

> **Town Policy — overrides formula instructions where they conflict.**

Pass a bead's text as a single-quoted argument on every `gt bead` verb that takes
text (`create`, `update`, `claim`, `note`, `comment` and `close`), across as many lines
as the text needs:

```bash
gt bead note gt-abc 'the retry loop never resets its counter, so a second
attempt spends the budget the first one started with'
```

The shell runs nothing inside single quotes, so a note about code keeps its
backticks and `$(...)` intact, and the tap guard reads the text exactly as
written. Beads are written with the `gt bead` verbs; `bd` is for reads.

When the text holds a single quote of its own it takes double quotes, where
`$`, backticks and braces expand: escape them so the text the guard reads still
opens literally.

## Tests stay hermetic (town policy, 2026-10-26)

> **Town Policy — overrides formula instructions where they conflict.**

Do not add tests to a package that `internal/testpolicy` lists as exempt (its baseline holds
only the tests it had at seed time), and do not run a subprocess, create an executable file or
run the Go tool inside a unit test: inject the runner and the filesystem and use fakes. Never
edit the baseline or raise a bound to land; a policy failure means the test is the problem.

## No new shell scripts (town policy, 2026-10-26)

> **Town Policy — overrides formula instructions where they conflict.**

Do not create `.sh`, `.bash`, `.zsh`, `.pl` or `.rb` files, and do not extend an existing
script with new logic. Write testable Go with unit tests instead. A procedure that is only a
sequence of operator steps is a runbook, not a script.

## Keep files under the worktree (town policy, 2026-10-27)

> **Town Policy — overrides formula instructions where they conflict.**

Files you create for the work stay inside your rig worktree: scratch files, trace
logs, test data and build output included. `/tmp`, `/private/tmp`, your HOME
directory and a session scratchpad are out — a file written there is outside what
the landing keeps, and the permission prompt its write raises in a parked session
has nobody to answer it (gt-3n4e0, hq-wisp-dvrty). Put scratch files in the
worktree's `scratch/` directory, which the checkpoint dog leaves untracked
(`internal/checkpoint/throwaway.go`).

## Scratch files stay out of the tracked tree (town policy, 2026-10-14)

> **Town Policy — overrides formula instructions where they conflict.**

The checkpoint dog commits your working tree into your branch every few minutes
("WIP: checkpoint (auto)"). A scratch test, harness or half-finished file left in the worktree
is committed within minutes, and `gt done`'s squash rewrites commit messages only, so the file
survives into the merge request even after you delete it. Keep scratch files out of the
tracked tree (the rule above places them in the worktree's `scratch/`), and if one slipped
in, delete it and commit the deletion before `gt done`.

## Tests are engineered, not accumulated (town policy, 2026-10-23)

> **Town Policy — overrides formula instructions where they conflict.**

A badly engineered test is a liability; two repos had to be rebuilt after theirs piled up.
Full standard: `~/.claude/docs/standards/test-engineering.md`. When you add or change a test:

- Test behaviour a caller can see, not how the code is built. One reason to fail per test.
- Give it a tier and a cost: unit is in-process and fast, integration is real processes or
  a sandbox. A test over its tier's cost moves up a tier or gets cheaper; it never gets a
  longer limit.
- Bound every wait and end every loop. No `sleep` to wait for something; use a fake clock or
  a condition. Read a clock baseline BEFORE you launch what you time.
- Never retry or skip a failing test to make it pass; find the cause.
- A test that runs a script or binary in a subprocess must itself read those files, or the
  result cache serves a stale pass after the script changes.
- Stage a fixture once and copy it per case; do not rebuild it for every case.
- Never raise a timeout, a budget or a ratchet to land your change. The lists only shrink.
- A shell test must pass on macOS too: the gate runs it on Linux CI, but the post-land tier runs it on the Mac host (BSD tools). Never compare `wc -l` output as a string (BSD pads it: trim or use `$((...))`), and avoid `sed -i`, `date -d`, `stat -c` and `readlink -f` without a portable form. A shell test that holds logic is also a candidate for Go (see the scripts-are-debt policy).

om flags the breaches above as major findings, and a new slow test as a finding even when it passes.

## No new scripts for testable logic (town policy, Sloan 2026-10-24)

> **Town Policy — overrides formula instructions where they conflict.**

Do not add a shell or Python script when the logic is testable Go. Write it as Go in the repo's
module with unit tests (a seam for the clock, the filesystem and the process runner). Wizards
stay outlawed. Allowed, with the reason in the bead: a thin wrapper that only execs a Go binary,
Makefile glue, a bootstrap that must run before Go exists, or a file the platform dictates.
If a bead asks for a script and gives no such reason, comment on the bead and ask before you
write it. A new script that holds logic is a major finding in om review. Full text: the "Scripts
are debt" section of `~/.claude/docs/standards/test-engineering.md`.

# What om rejects

Read before you write code. Sixteen rules. Today (2026-10-26) 70 beads were rejected in 138 blocks; 125 blocks (91 percent) had a major or critical finding. Each rule names om's finding and one example from today.

## 1. Failure is not success (fail-open)

A failed, unreadable or unwired input must stop, refuse or escalate, never pass as clean. This is the most common major finding. The forms om flags:
- an unreadable file, record or status read as absent, empty or clean (also an error from a build-constraint matcher or parser read as "does not apply");
- a refusal that returns the same value as a healthy answer (0, the base value, a nil error, exit 0): return a distinct error;
- a guard whose input is not wired defaults to allow ("PushInFlight gate is a constant false"): unknown denies;
- an error dropped silently, logged only, or reported as "skipped" where the caller has to act.
- om flags: "unreadable main status suppresses escalation (fail-open gate)".
- Example: gt-42ulv (critical). A missing red-main status read as green, so no escalation fired.
- Before: `if err != nil { return nil }`. After: return the error, or record the state as unknown and escalate.

## 2. Each criterion needs a test that fails without it

Every acceptance box needs a test that is red when the behaviour is removed.
- om flags: "criterion ... has no test".
- Example: gt-smdcj (major). A box said an acked item stays counted as acked, and nothing tested it.
- Before: the box ticked on reading the code. After: a test that fails when the ack check is deleted.

## 3. Test the branch that refuses, not only the happy path

Every error or refusal branch in the diff needs a test that drives it.
- om flags: "failing branch has no assertion", "alarm and refusal branches lack tests".
- Example: a new error return in internal/cmd with only the success case tested.
- Before: one test for the success path. After: one table row per refusal, each asserting the error.
- Before gt done, mutate each new guard yourself: delete its branch or flip its condition, and confirm a test goes red. A guard no test turns red is the commonest reason a head is rejected (56 findings in three days); if no test goes red, write the test.

## 4. Untrusted input is not trusted by its prefix or its text

Anything an author writes (a comment, a note, a bead field) can be forged. Decide authority from something the author cannot write.
- om flags: "decision authority is granted by prefix alone, so any commenter can forge a decision" (major).
- Example: gt-0sztd. A decision comment granted authority by its prefix, so any commenter could forge one.
- Before: `strings.HasPrefix(body, "DECIDED")` grants it. After: authority from the author's identity, checked on the record.

## 5. An author cannot review or rebut their own work

- om flags: "author under review can self-rebut its own findings" (major).
- Example: gt-yphcl. A round's rebuttal was accepted from the author whose findings it answered.
- Before: rebuttal accepted from any commenter. After: a rebuttal counts only from a reviewer or the overseer, not the author.

## 6. Verification claims show their output

A claim that something passed must paste the command and its output, not just say so.
- om flags: "verification claim has no shown output".
- Example: gt-p85pw.60 (minor). A research note said a check passed without the run's output.
- Before: "the gate passes". After: the command and its last lines of output.

## 7. Hermetic tests

A test must not read the developer's environment (SHELL, HOME, the live town tree) or depend on host state.
- om flags: "test depends on the host's $SHELL and prompt style, so it is not hermetic".
- Before: `os.Getenv("SHELL")` in the test. After: set the value in the test, or use a temporary directory.

## 8. Do not add a shell or Python script

Logic goes in Go with unit tests. A new .sh or .py file is refused.
- om flags: "new parsing and state logic added to a shell script". Before: a script that parses a file. After: the same parse as a Go function with a table test.

## 9. Stay in scope

Change only what the bead asks for. Unrelated files, dependency bumps and renames are findings.
- om flags: "edit to a file outside the bead's Constraints"; "existing test fixture edited to fit new behaviour"; "exported API signature changed without recording it"; "unexplained transitive dependency bumps". Earlier rounds quoted "change touches files outside the stated scope"; it still counts under this rule. Before: a fix that also rewrites an existing test. After: the fix alone, or a bead comment saying why the other file had to change.

## 10. Test the call site, not the helper

A test must run the production function that makes the call, so deleting the call turns it red.
- om flags: a test that passes with the logging call deleted.
- Example: gt-uoo5j. The hash-line test passed with the logging call removed.
- Before: a test that calls the helper directly. After: a test through the production caller, red when the call is deleted.

## 11. Keep secrets out of errors; tick boxes last

Never put captured output in an error message: name the rule and the command instead, since output can hold a secret.
- om flags: an error that echoes captured command output.
- Example: a guard error that printed the whole command output.
- Before: `fmt.Errorf("failed: %s", out)`. After: `fmt.Errorf("rule X: command failed")`.
- Tick every acceptance box only after the named test exists. gt done and the landing policy refuse unticked boxes, and a box ticked early is a finding.

## 12. Cloud authority widens only through a reviewed, visible diff

Applies to any change that touches a cloud project, a role, a service account or an API list.
- om flags: "role or permission added without a golden change", "role with an admin permission", "project-level grant", "enables an API the manifest does not list".
- Reject outright: a role or permission list widened without the matching change to its reviewed golden — the test of rule 2, "Each criterion needs a test that fails without it" — and a bead comment saying why; any *.admin permission or roles/owner, roles/editor, or a predefined role where a custom role fits; serviceAccountTokenCreator, serviceAccountUser, actAs or impersonation granted on a project instead of one account, or with no named owner; a service account or key created with no owner, rotation and deletion path stated.
- Before: a polecat adds iam.serviceAccounts.getAccessToken to a list. After: the permission is justified in the bead, the golden and the list change together, and dropping either one turns that test red.

## 13. A cloud apply needs a preview the human reads

"Cloud calls in a shell script" is refused by "No new scripts for testable logic (town policy, 2026-10-24)" and rule 8, "Do not add a shell or Python script"; this rule is what a cloud call adds on top of them.
- om flags: "apply with confirmations disabled", "preview gated by a grep".
- Reject outright: an apply, destroy or disable that runs without a preview step the code makes the human read; one that forces past that step (--yes, --force, --skip-preview, disableDependentServices); one with no typed confirmation; a destroy with no stated rollback.
- Example: dv-honu.6 (apply with confirmations disabled; the red-flag preview gate is a weak grep).

## 14. A step the diff cannot show must say what it creates

- om flags: "console-only step with no reconciliation", "side effects outside the diff".
- Reject outright: a bead or runbook that asks a human to click or run a provider flow (for example the console "add Firebase" step) without listing what the flow creates beyond the named result and the read-only command that reconciles it against the patrol.
- Example: 2026-10-30, the console Firebase step enabled 28 APIs and created firebase-adminsdk-fbsvc with Token Creator.

## 15. Cost and credentials are part of the change

- om flags: "billing plan or budget not stated", "key file or token on a command line".
- Reject outright: a new billable resource (VM, disk, address, SQL, load balancer, Blaze-only feature) with no budget alert named; a cloud credential a test requires or reads from its environment (rule 7, "Hermetic tests"), or one that is printed (rule 11, "Keep secrets out of errors; tick boxes last").

## 16. Rework: answer every open finding

A bead that comes back has a MERGE REJECTION block in its notes listing om's findings. Fix every one before you submit again, including any marked "still open" from an earlier round: om repeats them and the bead burns another round (35 findings in three days). Never loosen a test or a limit to land the fix (10 findings; the test-ratchet policy is in `~/.claude/docs/standards/test-engineering.md`). If a finding is wrong, say why on the bead with the evidence, and fix the others.
- om flags: "still open", "claimed resolved but not changed by the diff", "ratchet floor lowered to land the change", "existing test edited to fit the new behaviour".
- Before: delete the failing assertion, or lower the floor. After: fix the code, or comment on the bead why the finding is wrong.
