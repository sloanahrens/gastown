## Changelog

- 2026-10-27 gt-3vgbi: moved the shared host-hygiene rules and the testing norms into the repo from ~/gt/gastown/directives/_common.md; the testing norms are the byte-identical 21-line text (md5 fc0141bd7290b193679763095c7a00dc) that five rigs carry, so "golden files for prompts" is now in the gastown norms too.
- 2026-10-31 gt-pccx7: dropped the host-hygiene incident story, the CGO/ICU flags and the Test-load section (a polecat runs no full suite; the Makefile exports the flags).
- 2026-10-27 gt-3vgbi: the introducing bead of each rule is not recorded in its source file; the rules are cited here by the bead that moved them.

## Host Hygiene — shared workstation rules

> **Rig Policy — overrides formula instructions where they conflict.**
> This town runs on the operator's own 24-core Mac. Every agent shares its
> CPU, disk cache and antivirus scanner with the human who runs the town.

### Never scan the filesystem from `/` or `$HOME`
- `find / …`, `bfs / …`, `fd … /`, `grep -r / …`, `rg … /`, `du -s /`, `locate`-style walks
  are FORBIDDEN: one such walk took host load from 19 to 100.
- Root every search at the rig, the repo worktree, or a known prefix
  (`/opt/homebrew`, `$(go env GOROOT)`, `$(go env GOMODCACHE)`).
- For a header or library location ask the package manager (`brew --prefix <pkg>`,
  `pkg-config --cflags <pkg>`, `go env`), and build with the Makefile targets, which export the CGO flags.


# Testing norms (rig directive)

Applies to every agent working this rig, whatever formula is running.

- Changed behavior ships with tests at the repo's established seams: table-driven
  units for logic, golden files for prompts, stub-backend e2e for CLI behavior.
  No model calls in tests, ever.
- A bug fix includes a test that fails before the fix and passes after.
- Tests assert external behavior (outputs, exit codes, files written), never
  implementation details.
- While you work, run only the one test or file that covers what you just
  changed. Never run the same command twice without an edit in between.
- When you are done, run `gt done` once: it runs this rig's presubmit gate on
  your branch, and the landing then runs the full gate on the merge candidate.
  Do not run `make test`, `make lint` or `make gate` yourself. om weighs test
  quality via the repo rubric (.om.json).
- If `gt done` reports red, fix the failure it names and run `gt done` again.
- A failing test your diff does not touch has a cause. File a P1 `Found:` bead
  (test name, full output, how you ran it) and run `gt done --status DEFERRED`.
  Do not retry it, and do not add retries, sleeps or longer timeouts to make
  it pass.
