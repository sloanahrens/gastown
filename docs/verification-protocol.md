# Verification Protocol

> **Rig Policy — overrides formula instructions where they conflict.**

This document canonizes the witness's 7-step verification protocol as the
authoritative reference for verification instruments and destruction-adjacent
changes in gastown.

## The Seven Steps

The verification protocol is a sequential checklist that must be followed
exactly when making changes to verification instruments or destruction-adjacent
code. Each step is mandatory; skipping or reordering steps defeats the
protocol's purpose.

### Step 1: Pre-register criterion incl. INCONCLUSIVE before the fix exists

Before implementing a fix, register all possible outcomes as alternative
hypotheses, including INCONCLUSIVE. This prevents confirmation bias by
forcing the verifier to specify what would constitute evidence against each
hypothesis.

**Purpose:** Establish a baseline of what constitutes evidence for/against
each possible state before observing the system.

### Step 2: Verify binary in the same capture (version + ancestry + git cherry for rebases)

The binary under test must be verified in the same capture event as its
version, ancestry, and git cherry-pick status. This ensures the binary being
tested is exactly the one claimed, not a different build or a rebase artifact.

**Purpose:** Prevent confusion between the binary that was built and the
binary that is actually running.

### Step 3: Verbatim capture with ground truth from artifacts OTHER than the instrument under test

Capture all observables verbatim from artifacts that are independent of the
instrument under test. The ground truth must come from a separate source to
avoid instrument bias.

**Purpose:** Eliminate instrument-specific artifacts from the verification
evidence.

### Step 4: Report provisional, naming the unexcluded alternative

Report findings as provisional, explicitly naming which alternative hypothesis
has NOT been excluded by the observation. Do not declare a "winner" until all
alternatives have been tested.

**Purpose:** Prevent premature conclusions by documenting what remains
possible.

### Step 5: Exclude alternatives by observation, not argument

Exclude alternative hypotheses only through direct observational evidence,
never through argument or inference. If you cannot observe a difference, the
alternative remains viable.

**Purpose:** Prevent circular reasoning where the conclusion influences what
is considered evidence.

### Step 6: Final only then

Do not declare a final conclusion until all alternatives have been excluded
by observation. The conclusion is final only at the moment all evidence
converges.

**Purpose:** Ensure conclusions are robust and not based on incomplete
evidence.

### Step 7: Continuous sampling across the subject's lifetime

Verify across continuous sampling of the subject's entire lifetime, not just
at discrete points. Structural defects often appear only during transitions.

**Purpose:** Distinguish between boundary cases (which terminate) and
structural defects (which persist). Only continuous sampling reveals what
does NOT terminate a state.

## The Property: INVARIANCE

The verification protocol buys **INVARIANCE** — the property that the
verification instrument produces consistent results regardless of when or
how often it is run. Only continuous sampling (Step 7) shows what does NOT
terminate a state, which is what distinguishes structural defect from
boundary case.

## Scoping

The protocol applies to:
- Changes to verification instruments
- Destruction-adjacent code

**Provenance:** This protocol was earned step-by-step from the following
failures:
- Amber: 14-minute invariance failure
- Marble: 54 samples with missing boundary case
- The 5x gap between merged vs. installed binary
- The startup-window alternative

## Cross-references

- Refinery gate policy: see `contrib/gastown/directives/refinery.md`

## Do NOT

- Skip any step or reorder the sequence
- Declare a final conclusion before all alternatives are excluded
- Use argument or inference to exclude alternatives
- Sample at discrete points only — continuous sampling is mandatory
