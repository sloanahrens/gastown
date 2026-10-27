## Changelog

- 2026-10-27 gt-3vgbi: moved the shared host-hygiene rules and the testing norms into the repo from ~/gt/gastown/directives/_common.md; the testing norms are the byte-identical 21-line text (md5 fc0141bd7290b193679763095c7a00dc) that five rigs carry, so "golden files for prompts" is now in the gastown norms too.
- 2026-10-27 gt-3vgbi: the introducing bead of each rule is not recorded in its source file; the rules are cited here by the bead that moved them.

## Host Hygiene — shared workstation rules

> **Rig Policy — overrides formula instructions where they conflict.**
> This town runs on the operator's own 24-core Mac. Every agent shares its
> CPU, disk cache and antivirus scanner with the human who runs the town.

### Never scan the filesystem from `/` or `$HOME`
- `find / …`, `bfs / …`, `fd … /`, `grep -r / …`, `rg … /`, `du -s /`, `locate`-style walks
  are FORBIDDEN. On 2026-09-09 one `bfs / -name regex.h` ran 8 minutes at 500% CPU,
  took host load from 19 to 100 and made the operator's terminal unresponsive.
- Root every search at the rig, the repo worktree, or a known prefix
  (`/opt/homebrew`, `$(go env GOROOT)`, `$(go env GOMODCACHE)`).
- If you need a header or library location, ask the package manager:
  `brew --prefix <pkg>`, `pkg-config --cflags <pkg>`, `go env`.

### CGO / ICU (the case that caused the incident)
- `go test ./...` and `go build` in this repo fail on `unicode/regex.h` without ICU flags.
- Do NOT search for the header. Use the Makefile targets (`make build`, `make test`),
  which export the flags, or set them yourself:
  `CGO_CPPFLAGS=-I$(brew --prefix icu4c)/include CGO_LDFLAGS=-L$(brew --prefix icu4c)/lib`

### Test load
- A polecat does not run the full suite (see Testing norms below). If a task truly
  needs one, run it as `GOFLAGS=-p=6 make test`, never bare `go test ./...` at full parallelism.
- Before a full suite: `top -l 3 -n 0 -s 2 | grep 'CPU usage' | tail -1` — proceed only if
  idle >= 25%; otherwise wait 2 minutes and re-check.
- Anything that outlives a tool call runs as a tracked background task, never `nohup … &`
  inside a foreground call (it is killed when the turn ends).
- One full suite per agent at a time.

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
