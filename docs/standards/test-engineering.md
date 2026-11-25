# Test engineering standard (all repos)

> Canonical copy since 2026-11-24 (moved from the operator's ~/.claude/docs/standards so a sandbox seat can read it); edit it here.

Applies to every test in every town repo, written by agents or by hand (operator, 2026-10-23, bead claude-hb9). A badly engineered test is a liability: the devops and gastown suites each had to be rebuilt after theirs accumulated. A test earns its place by catching a real regression cheaply, deterministically, and in a way that tells you what broke; a test that is slow, hangs, depends on timing, goes stale or checks how the code is built is debt, and review treats it as debt. Each rule carries the evidence it came from.

## The rules

1. **Test behaviour, not construction.** Assert what a caller or user can observe. A test that fails when code is refactored without changing behaviour, or that passes when behaviour breaks, is a liability. One reason to fail per test.
2. **Every test has a tier, and the tier has a cost.** Unit tier: in-process, hermetic, fast. Integration tier: real processes, sandboxes, containers, network. A test over its tier's cost moves to the larger tier or gets cheaper; it is never given a longer limit. (Google test sizes and Bazel timeout classes work this way: exceeding the class means recategorise.) The repo's own docs/testing.md says the numbers; where a repo has none, a unit test is under a second and anything that launches a process or touches a network is integration.
3. **Every wait is bounded and every loop ends.** No unbounded polling, no `while` that depends on something outside the test to stop. Hang detectors exist at test level, not only suite level: bats `BATS_TEST_TIMEOUT`, a per-test `context` deadline, `go test -timeout`. A hang must fail one named test in about a minute, not spend the gate's ten.
4. **Deterministic under any load.** No `sleep` to wait for something, no reliance on wall-clock ordering, scheduling or machine speed. Use a fake clock, a condition to wait on, or `testing/synctest`. Never drive a real polling loop from a fake clock, and read the clock baseline BEFORE launching what you time. Evidence: scripts/release `run_under_bound` read `start` after launching a fake that moves the clock; under load the test went 0.7 s to 25-71 s two times in three and was the gate's 10-minute kill (dv-lqrk).
5. **A test that fails sometimes is a bug, and it is fixed.** No retry-to-pass, no skip-to-pass, no "flake" allowance. Find the cause. If the cause is the host's load, the test depends on speed and breaks rule 4.
6. **Cache-honest: declare your inputs.** A test that runs a script, binary or fixture in a subprocess must itself read (open, hash) those files so the language's result cache sees them change. Verified 2026-10-23: a Go test that only exec's a script stays `(cached)` after the script changes (a stale pass); the same test reading the script first is invalidated. The rule applies wherever a runner caches or skips by input (Go's test cache, Gradle, Bazel, a selective-run script that hashes inputs): the inputs it hashes must include everything the test's subprocesses read. bats and plain shell suites have no result cache, so there the rule is: never skip a suite on a hash that omits the scripts it runs. A repo whose tests cannot declare inputs must not put them behind a cache. A test that LISTS files (`git ls-files`, a directory walk) must also open (`os.ReadDir`) the repository root and every directory holding a listed file: Go hashes an opened directory's entry names, so an added file changes the cache key, while the git subprocess itself is invisible to the cache. Verify it with the plain test (no `-count=1`) in a scratch worktree after backdating every file and directory mtime (`find . -path ./.git -prune -o -type f -exec touch -t 202611010000 {} +`, then `find . -path ./.git -prune -o -type d -exec touch -t 202611010000 {} +` for the directories): expect `(cached)`, add an untracked file, expect a re-run. Right after a fresh checkout Go caches nothing, because the inputs are minutes old, so an unbackdated check proves nothing. Evidence: dv-ksqh.1 comment of 2026-11-05 (guard f01cdfda).
7. **Cheap per case.** Do not rebuild a fixture, sandbox, checkout or tarball for every case: stage once, copy per case. Count the processes a case spawns; dozens per case across hundreds of cases is the cost (devops harness family: about 270 cases, 78 s user and 92 s system CPU, 54-67 s of every landing's gate). Do not copy-paste a case into a family that multiplies cost.
8. **Hermetic.** No network except local fakes, no writes outside a temp directory, no shared mutable state between tests, no reading the developer's real home, no cloud. Tests run in any order and in parallel. `t.TempDir()` hands out a subdirectory made with mode 0777 minus the umask (0755 in a seat, 0777 in a CI job with an open umask): a test that needs a private directory chmods it to 0700 first.
9. **A failure explains itself.** The message names the cause and carries the tail of the relevant output, so it can be fixed without a rerun.
10. **Enforced, not remembered.** Where a repo has a policy test, a budget or a ratchet, new work meets it from its first commit. You never raise a timeout, budget or ratchet to land a change; the list only shrinks. If a limit is genuinely wrong, raising it is its own change with the measurement that shows it is wrong. The integration cap (`testpolicy.SlowTestCap`) needs no separate approval; every other limit still needs explicit human approval, recorded on the bead. Where a repo has none, the reviewer holds these rules.

## Scripts are debt: write testable Go, not a script (operator, 2026-10-24)

General policy for every town repo, written by agents or by hand: **do not add a shell, Python or other script when the logic is testable Go code.** A script cannot be unit tested, mocked or run in the fast tier; its test is a second script (bats or a stub harness) that carries the cost and the flake risk this standard exists to remove. Wizards (interactive driver scripts) are outlawed outright (operator 10-21).

- **Write it in Go**, in the repo's module, with the rules above: a seam for the clock, the filesystem and the process runner, so the logic is a unit test and the real command runs once, in the integration tier.
- **Allowed, with the reason in the bead:** a thin wrapper (a few lines) that only execs a Go binary or sets an environment; build and CI glue that is already a Makefile target; a bootstrap that must run before Go exists or inside a guest that has no Go; a file the platform dictates (a launchd plist, a hook entry, a Dockerfile). The reason names why Go cannot do it.
- **A bead does not ask for a script deliverable** unless it states that reason. A polecat that finds the work would be a script files the question on the bead before writing it.
- **Reviewers (om and humans):** a new script that holds logic is a MAJOR finding ("port to Go with unit tests"); a new bats/stub test for it is MINOR on top. An existing script is not ported by drive-by: file a bead per script, small enough to land (the list of candidates is kept on claude-hb9).
- **Inventory first:** scripts added recently and now candidates to port: deploy/k8s/kind/{up,down,common}.sh (+ kind_test.sh), scripts/openshell-providers.sh, plugins/polecat-image/run.sh, scripts/install-gt.sh and its test, the devops release scripts (dv-fec7 is already porting them).

## For authors

- Before adding a test, say which tier it is and what it costs. Measure with the repo's tool (`go test -json`, `bats -T`), not a guess.
- A new slow test is a finding in review even when it passes.
- A shell test runs on Linux in CI and on the Mac host in the post-land tier: it must pass on both. BSD vs GNU traps seen: `wc -l` pads its number on macOS (compare numerically), `sed -i`, `date -d`, `stat -c`, `readlink -f` (2026-10-24: the town-metrics overlap assertion failed on every macOS run and passed on Linux CI, red main gt-sj7zs).
- If the honest way to test something is slow, it is integration tier with a trigger so it runs when its inputs change, plus a sweep; do not leave it in the fast gate.

## For reviewers (om and humans)

Flag as MAJOR: an unbounded wait or loop; a timeout, budget or ratchet raised to make a change land; a test that exec's a script or binary without reading it (stale cache); sleep-based synchronisation; a retried or skipped failing test; a clock baseline read after the launch it times; a test asserting implementation details so that a refactor breaks it.
Flag as MINOR: a test over its tier's cost; a fixture rebuilt per case; a new family of near-identical slow cases; a failure message that names no cause; shared state or ordering between tests.
Phrase test findings as static hypotheses from the code ("this loop has no bound"), never as "the test fails when run": a reviewer cannot execute (gt-jq95).

## Where this is enforced

- Mechanically, today: gastown and devops have `internal/testpolicy` (the rules, an only-shrinking `unconverted.txt`, a user-CPU budget). Other repos rely on review.
- By review: each repo's `.om.json` rubric has a `test-engineering` criterion with the reviewer list above. Rollout is tracked on claude-hb9; a repo without the criterion is held to these rules by its human or agent reviewer until it lands.
- By instruction: `~/.claude/CLAUDE.md` (every interactive session), `~/gt/CLAUDE.md` and the town polecat directive (`~/gt/directives/polecat.md`, which carries a copy of the rules; this document wins if they differ).

Evidence and history: `~/.claude/docs/lessons/slow-tests-recur-across-rigs.md`, `~/.claude/docs/research/claude-hb9/devops-test-speed-2026-10-23.md`.
