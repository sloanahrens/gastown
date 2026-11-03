> Status: plan (2026-11). Design for the bead-lint grammar changes; approved by the operator on 2026-11-03. Tracked by epic gt-hgt9j and decision bead gt-62yz9.

# Bead lint v2: the grammar group

Source: docs/research/2026-11-bead-linting.md, ideas 1, 2, 6, 9 and 12 plus three decisions. The eight small ideas (3, 4, 5, 7, 8, 10, 11) are already beads under epic gt-hgt9j and are not repeated here.

## Decisions (all approved)

1. **`## Files` sits beside the cue words.** An optional section lists `create:`, `modify:` and `read:` paths, one per line. A bead that has it uses it as the only source for the path check; the cue grammar is not applied to its Constraints. A bead without it keeps today's cue grammar. No backfill. The cue grammar is retired later, when almost every open bead has the section.
2. **Dependency-aware inventory.** A `create:` path of the bead itself passes, and so does a path that a transitive open blocker lists under `create:`. A path nobody creates stays refused. Only beads with `## Files` get this.
3. **`gt spec lint --graph <epic>`** is a separate command for filers and the overseer, not part of per-bead lint or the dispatcher tick. An optional `## Interfaces` section carries `Defines:` and `Uses:` lines of identifiers. The graph check refuses (exit 1) a name defined by two beads, a used name no blocker defines, and two independent beads that modify the same file. A test name (the leading `TestX:` of an acceptance box) repeated across the epic is an advisory.
4. **Size from content** is advisory only: warn when `## Files` lists more than 6 files. The threshold is calibrated after the lint outcome log (gt-hgt9j.7) has a week of data joined to overrun data; the bead is filed deferred.
5. **Lint at creation** is a PostToolUse hook in `~/.claude`, not a change to `bd`: after a `bd create` of a non-epic bead it runs `gt spec lint <id>` and shows the result. Label inheritance in `bd` is left alone; the advisory of gt-hgt9j.6 covers it.
6. **The writing guide** is `docs/guides/writing-a-bead.md`: the six lessons and the `Files` and `Interfaces` syntax. The template file `~/.claude/docs/agents/spec-template.md` does not exist and stays absent, because creating it would change the required sections. The spec-dispatcher guide and the lint's refusal text point to the guide.
7. **`shape_gate` moves from `warn` to `refuse`** as a one-line config change by the overseer after gt-hgt9j.1 to .4 have landed, with a dry-run list of the beads it would refuse first. The waiver label has never been used.
8. **Overseer-run beads.** A bead wearing `overseer-only` or `operator-only` is still linted for sections and boxes; for it the Size rule also accepts "one session of the overseer" or "one session of the operator".

## Syntax

```
## Files
create: internal/update/state.go
create: internal/update/state_test.go
modify: internal/specdispatch/lint.go
read: docs/guides/spec-dispatcher.md

## Interfaces
Defines: Store, Store.Mutate, ErrNoState
Uses: Clock
```

One path per line; a line that is not blank, not a heading and not `verb: path` is a refusal naming the line. Verbs are lower case. `read:` paths must exist on the base; `modify:` paths must exist on the base or be created by an open transitive blocker; `create:` paths need nothing.

## Order

The small chain gt-hgt9j.1 to .8 first. Then the Files parser and the path check, then the inventory, then the graph pieces, then the guide and the hook, then the `shape_gate` change and the size advisory. Details and the beads are in the plan.

## Not decided here

How the dashboard shows lint outcomes, whether the dispatcher tick should record outcomes (needs a change-only rule), and when the cue grammar is removed.
