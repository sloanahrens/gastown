> Status: plan (2026-11). Implementation plan for docs/plans/2026-11-03-bead-lint-v2-design.md; the beads under epic gt-hgt9j are authoritative once filed.

# Bead lint v2 plan

Spec: docs/plans/2026-11-03-bead-lint-v2-design.md. Epic: gt-hgt9j (children .1 to .8 exist already). Every task is one bead: twenty minutes of one polecat, narrow `-run` gates, no container-suite package run whole. All tasks that edit internal/specdispatch are a chain (each lands before the next dispatches). Hand-shaped, hand-slung by the overseer.

## Beads

| Id | Title | Depends on | Files | Risk path |
|---|---|---|---|---|
| F1 | `## Files` parser | gt-hgt9j.8 | internal/specdispatch/files.go (new) | no |
| F2 | path check reads Files when present | F1 | internal/specdispatch/target.go, lint.go | no |
| F3a | blocker-created paths pass (specdispatch side) | F2 | internal/specdispatch/target.go | no |
| F3b | the caller fills blocker-created paths | F3a | internal/cmd/spec.go | no |
| K1 | Size rule for overseer-only and operator-only beads | F3b | internal/specdispatch/lint.go | no |
| G1 | `## Interfaces` parser | K1 | internal/specdispatch/graph.go (new) | no |
| G2 | graph checks (pure) | G1 | internal/specdispatch/graph.go | no |
| G3 | `gt spec lint --graph` command and test-name advisory | G2 | internal/cmd/spec_graph.go (new), internal/cmd/spec.go | no |
| W1 | `docs/guides/writing-a-bead.md` and pointers | G3 | docs/guides/writing-a-bead.md (new), docs/guides/spec-dispatcher.md | no |
| Z1 | size advisory over 6 files (deferred until the L7 log has a week) | G3 | internal/specdispatch/advice.go | no |

Overseer-run (not polecat beads): **H1** the creation hook (a PostToolUse entry in `~/.claude/settings.json` that runs `gt spec lint` on a new bead id; reviewed with `om review -base origin/master`; Sloan's-name and attribution rules apply); **S1** the `shape_gate` change after gt-hgt9j.1 to .4 landed: run `gt spec dispatch --dry-run`, list what `refuse` would label, set `polecat_pool.shape_gate` to `refuse` in the town config, read it back.

## Acceptance boxes

**F1.** TestFilesParseVerbs (three verbs, many paths); TestFilesParseRefusesBadLine (names the line number and text); TestFilesParseAbsentSection (returns "no block", not an error); TestFilesParseIgnoresBlankLines; TestFilesParseRejectsUpperCaseVerb.

**F2.** TestPathCheckUsesFilesWhenPresent (a Constraints path with no cue is not checked when Files exists); TestPathCheckFilesReadMustExist (a `read:` path absent on base is refused); TestPathCheckFilesModifyMustExist; TestPathCheckFilesCreateNeedsNothing; TestPathCheckWithoutFilesUnchanged (cue behaviour identical to before).

**F3a.** Spec gains `BlockerCreates []string`. TestModifyPassesWhenBlockerCreates; TestModifyRefusedWhenNoBlockerCreates; TestReadPassesWhenBlockerCreates; TestBlockerCreatesIgnoredWithoutFiles (cue beads do not get it); TestRefusalNamesTheMissingBlocker (the Reason says no open blocker creates the path).

**F3b.** The caller walks the bead's open blockers transitively (cycle-safe, capped depth 8), parses each blocker's Files, collects `create:` paths. TestBlockerCreatesWalksTransitive; TestBlockerCreatesSkipsClosed; TestBlockerCreatesCycleSafe; TestBlockerCreatesReadFailureFailsClosed (an unreadable blocker is a refusal naming it, never an empty set).

**K1.** TestSizeAcceptsOverseerSession for a bead labelled `overseer-only`; TestSizeAcceptsOperatorSession; TestSizeStillRefusesOrdinaryBeadSayingOneSession; TestKindUsesSameLabelCompareAsHasLabel.

**G1.** Parses `Defines:` and `Uses:` lines into identifier lists. TestInterfacesParse; TestInterfacesRejectsNonIdentifier; TestInterfacesAbsent; TestInterfacesMultipleLinesMerge.

**G2.** `CheckGraph(beads []GraphBead) []GraphFinding` where GraphBead carries id, depends-on ids, Defines, Uses, modify and create paths. TestGraphDuplicateDefine; TestGraphUseWithoutBlockerDefine (transitive blockers count); TestGraphIndependentBeadsSameModify; TestGraphDependentBeadsSameModifyAllowed; TestGraphCleanEpic.

**G3.** The command reads the epic's children, builds GraphBeads, prints findings, exits 1 on any finding and 0 otherwise; the duplicate-test-name advisory prints without changing the exit. TestSpecGraphCommandExitCodes; TestSpecGraphTestNameAdvisory (the leading identifier of each box before the colon); TestSpecGraphJSON; TestSpecGraphUnreadableChildFailsClosed.

**W1.** Docs only: first line a `> Status:` header, `make docs-lint` passes; the guide has the six lessons, the `Files` and `Interfaces` syntax, and a worked example; spec-dispatcher.md links to it; the path refusal text of gt-hgt9j.1 names it.

**Z1.** TestSizeAdviceOverSixFiles; TestSizeAdviceSixFilesSilent; TestSizeAdviceNeedsFiles. Filed deferred.

## Dispatch order and checks

gt-hgt9j.1 to .8 (already filed), then F1, F2, F3a, F3b, K1, G1, G2, G3, W1; Z1 deferred. After every landing: run `gt spec lint` over three known-good beads (gt-o4sk8.2, .9, .19) to prove nothing newly refuses them; after gt-hgt9j.3 and .4 and K1, run `gt spec dispatch --dry-run` and record any bead that became unshaped. S1 follows .4. H1 whenever the overseer has a quiet slot.

## Later tracks

Retire the cue grammar once the dry-run shows every open bead has `## Files`. Record outcomes from the dispatcher tick with a change-only rule. Show lint outcomes on the dashboard.
