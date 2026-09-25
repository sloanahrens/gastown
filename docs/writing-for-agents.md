# Writing for agents

The standard for every document and comment in this repository. The audit applies every rule to every file in its slice; om weighs new and edited text against them; a polecat reads this before writing any comment, directive, formula, plugin.md, or doc.

Each rule is one checkable line. The paragraph under it says why. Rule ids are stable; cite them in commit messages and findings.

## Tiers

- **Agent-facing**: AGENTS.md, `internal/templates/polecat-CLAUDE.md`, `docs/HOOKS.md`, `plugins/*/plugin.md`, the description blocks of `internal/formula/formulas/*.toml`.
- **Reference**: README.md and every file under `docs/` that is not historical.
- **Historical**: every file under `docs/plans` and `docs/research`, and any `docs/design` file whose first non-blank line is a `> Status:` header.
- **Go**: doc comments on exported identifiers and inline comments in non-test `.go` files.

The authoritative file lists are `scripts/docs-lint.sh --list <tier>`. This section describes them; it does not duplicate them.

## Rules

### Pointer

**R1. A line that names another document states what it is and when to read it, trigger word first.**

The wording of a pointer, not its target, decides whether an agent reaches the material. "Before editing any formula, read X" fires; "see also X" does not.

### Single source of truth

**R2. Each meaning lives in exactly one place. A second copy becomes a pointer to the first.**

Two copies drift. The reader who finds the stale one has no way to know it is stale.

### Cache

**R3. A doc or comment that restates `--help` output, a config file, the Makefile, or the directory layout is deleted.**

The environment is the source of truth for those, and it cannot go stale. Keep what a lookup cannot find: the convention, the reason, the gotcha.

### No-op

**R4. A sentence the agent would already obey by default is deleted whole.**

It spends context to change nothing. Trim words from it and it still changes nothing.

### Sediment

**R5. A line describing behaviour that no longer exists is deleted, not amended.**

Adding feels safe and removing feels risky, so stale layers settle until the live line is buried. Deletion is the only cure.

### Sprawl

**R6. An agent-facing file over 1,500 words is split by branch, or its reference material moves behind a pointer.**

Attention thins across a long file. Inline what every reader needs; push behind a pointer what only some readers reach. (The lint gate enforces a looser 2,000 on the always-loaded files; this rule is the audit's target.)

### Completion criterion

**R7. Every step in a formula or directive ends with a condition an agent can check.**

"Understanding reached" invites stopping early. "Every modified file accounted for" forces the legwork.

### Negation

**R8. State the target behaviour. A prohibition stands only as a guardrail, with the positive form beside it.**

A ban names the thing it bans and makes it more available, not less.

### Comments

**R9. A comment says why. It states what the code visibly does only when the why is in the same sentence.**

The code already says what. A comment that repeats it is a cache (R3) that rots the first time the code changes.

**R10. A doc comment on an exported identifier states the contract in one sentence.**

Callers read the contract. Implementation detail belongs in the body.

**R11. Narrative history collapses to one bead-id citation. The citation replaces the story; it never accompanies it.**

`bd show` holds the story. `// kept at 60m so gate slots are not starved (gt-uoqg)` is complete; three sentences of what it used to be are sediment (R5).

### Historical docs

**R12. A historical doc starts with `> Status: historical (<YYYY-MM>). <Superseded by | Merged in | Abandoned>: <ref>. Not maintained.`**

The header is what stops an agent citing a 900-line proposal as current truth, and it is the classifier `scripts/docs-lint.sh` uses for the tier.

### The test

**R13. A line that changes nothing an agent or reader does is deleted.**

This is the rule behind the others. When a line survives every rule above, ask this one.

### Observed and static claims

**R14. A claim that code fails at runtime names the run that observed it. Without a run, state the defect and its location as a static hypothesis.**

An agent that ran the suite reports what it saw and can point at the command. A reviewer that has only read the tree and the diff — the om editorial gate executes nothing (gt-jq95) — has no run to cite, so "the tests fail" is unfalsifiable from where it stands; operators discount a finding that overstates certainty. "This will fail when run because `<defect>` at `<path>:<n>`" is the same claim with evidence the reader can check.

## Applying the rules

An audit run records, per file, which rule ids produced edits. A finding it cannot act on (a deletion proposal, rot outside its slice, a rule that needs sharpening) goes in the run bead comment, not in the file.
