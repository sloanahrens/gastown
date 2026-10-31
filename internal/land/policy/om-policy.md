# Town policy om weighs at landing

These are the rules the town holds a change to. Each breach is a finding that
names the rule and the line it breaks, so a reader can check it.

## Unit tests run in-process

A unit test uses fakes, not subprocesses: it does not run a script, another
binary or the go tool, and it does not set a package-level variable, production
or test, to steer behaviour. The process runner, the filesystem and the clock
are injected seams the test scripts.

Source: test standard, rules 4 and 8 (~/.claude/docs/standards/test-engineering.md).

## Testable logic is Go, not a script

Logic ships as Go in the module with unit tests, never as a new `.sh`, `.bash`,
`.zsh`, `.pl` or `.rb` file, and an existing script does not grow new logic. An
exception holds only where the bead states it: a thin wrapper that only execs a
Go binary, Makefile glue, a bootstrap that must run before Go exists, or a file
the platform dictates.

Source: test standard, "Scripts are debt", and the polecat directive's town-policy section "No new scripts for testable logic".

## A test has a tier and a cost

Unit is in-process and fast; integration runs real processes or a sandbox. A
test over its tier's cost gets cheaper or moves up a tier; a longer timeout or a
bigger budget is not a fix. Every wait is bounded and every loop ends, and a
test reads its clock baseline before it launches what it times. A new slow test
is a finding even when it passes, and a failing test is fixed or found to be
wrong, never retried or skipped to pass.

Source: test standard, rules 2, 3, 5 and 7 (same file).

## Baselines and ratchets only shrink

An exemption list, timeout, budget or ratchet is never raised to land a change,
and a bound is never widened for it. A policy failure means the test is the
problem. A new exemption carries its reason, and the lists only shrink.

Source: test standard, rule 10 (same file).
