## om Editorial Gate Policy

> **Rig Policy — overrides formula instructions where they conflict.**

This rig runs `om` as a named pre-merge editorial gate (**om-editorial**),
invoked exclusively through `gt mq review`. Every merge request is
editorially reviewed before merge; a `request_changes` verdict blocks the
merge and rides the existing FIX_NEEDED loop back to the worker. Quality
review in this rig is NOT measurement-only.

## Verification Protocol

For changes to verification instruments and destruction-adjacent code, the
witness's 7-step verification protocol must be followed. See
`../../docs/verification-protocol.md` (relative to this file) for the
canonical protocol. The protocol buys INVARIANCE — only continuous sampling
shows what does NOT terminate a state, which is what distinguishes
structural defect from boundary case.

### The gate contract

### The gate contract

`gt mq review <mr-bead-id> --rehearsed temp` invokes `om` (through the
backend-agnostic `om-gate.sh` script) and carries the verdict in its exit
code:

| Exit | Meaning | Refinery action |
|------|---------|-----------------|
| 0 | approve | proceed with the merge |
| 1 | request_changes — editorial rejection | abort merge, FIX_NEEDED to the polecat (`Failure-Type: om-editorial`) |
| 2 | infra error (always fails closed) | skip the MR, escalate to Witness, do NOT send FIX_NEEDED |

### Failure classes (exit 2)

Every non-verdict outcome is classified before the gate fails closed.
Routing is on exit code and `failure_class`, never on log text — this
table is the single source of truth for what each class means; do not
restate or narrow it elsewhere.

| failure_class | detected by |
|---|---|
| `binary_missing` | `om` not on PATH or not executable |
| `version_mismatch` | om binary sha ≠ manifest, `om version` < `min_version`, or `.om.json` sha ≠ manifest |
| `config_error` | om stderr begins `om: config error:` or `.om.json` fails schema |
| `backend_timeout` | om exit 2 with timeout marker, or wrapper deadline (`timeout` + 60 s) |
| `malformed_verdict` | verdict file absent, 0 bytes, or fails the verdict JSON schema |
| `tooling` | mktemp, jq, git, or note write failing before a verdict |
| `record_failed` | note or receipt write failed after a verdict was obtained |
| `precondition` | push refused: note missing, patch-id mismatch, or version below minimum |

`record_failed` blocks even though a verdict exists: a merge without a
durable note would fail the push precondition anyway, and a verdict
without proof is exactly the defect this gate exists to remove.

### Findings routing

The verdict is recorded as a **comment on the MR bead** — the rejection
summary and findings, and approvals too, so every verdict has a durable
record. Your FIX_NEEDED mail already carries `MR-Bead-ID`; point the
worker at `bd show <mr-bead-id>` for the findings. Do not paraphrase
findings into the mail body — the bead comments are the record.

### Proof of review

`gt mq review` writes a git note (`refs/notes/om`) on the reviewed head
and a receipt bead, in that order, on every verdict. The note is the
proof that survives DB flattens, migrations, and MR-bead deletion; the
receipt feeds per-worker aggregation. Neither is something you record by
hand, and `om-gate.sh` no longer records anything itself — it only
invokes om and routes findings to the MR bead.

### Fail-closed boundary

An unobtainable verdict is infrastructure trouble, never a verdict
itself. The gate fails closed: it logs the failure class, records a
failure receipt, and exits 2 so an unobtainable verdict can never be
mistaken for an approval (om-yed). This trades throughput for safety —
sustained infra trouble stalls the queue instead of admitting unreviewed
merges, which is the accepted tradeoff. A fail-closed skip is always
logged as such — if you see "fail-closed" in gate output, escalate to the
Witness per the table above and mention it in your merge summary.

### Escalation evidence requirements

An exit-2 escalation is only as useful as the evidence in it. Every
`version_mismatch` or `backend_timeout` escalation carries, alongside the
Witness escalation:

- **The IDs you actually read** — the MR bead, the gate's failure receipt,
  and the `refs/notes/om` note on the reviewed head — as IDs, not a
  summary of them.
- **The versions observed**: the installed om binary sha against the
  manifest's `om_binary.sha256`, and the rig's rubric pin against the
  rubric sha actually seen.
- **The measurement window**: the start and end timestamps of the run that
  failed, and the budget in force (the wrapper deadline).

Rationale: a body that says only "version_mismatch suggests the om harness
is out of sync" (hq-wisp-0g6094) makes the reader re-derive what the
reporter already held. IDs, shas, and a window make the claim checkable in
one step, and separate a non-reproducible transient from a live defect.

**Re-measure before re-filing.** Re-check the CURRENT budget and the
CURRENT shas before filing: the installed sha may already match the
manifest, and a rig's budget may already have been raised — om's review
timeout went 300s -> 900s (om-cwy). A failure at the CURRENT setting is new
evidence; the same failure at a setting that has since changed is not, and
re-filing it unchanged is noise.

### Do NOT

- Merge an MR whose om gate exited 1.
- Re-review the diff yourself when the gate is installed — om is the
  editorial authority; your job is routing its verdict.
- Treat exit 2 as an approval by om.
- Invoke `om` or `om-gate.sh` directly — only `gt mq review` (or, on the
  single-MR path, `gt mq verify` — see below) produces a durable, auditable
  record.

## Single-MR path: `gt mq verify` overlaps the suite and the review

mol-refinery-patrol's verify-and-review step calls `gt mq verify`, not `gt mq
review` directly. It runs the quality checks/test suite and, when
`merge_queue.editorial.required` is set, the om review as two goroutines
under one context inside the refinery engine (internal/refinery/overlap) —
concurrently, not the suite then the review in series. Internally it still
calls `editorial.Run` (the same code `gt mq review` calls), so the review
itself — rehearsal-free ref-only invocation, version assertion, rubric
guard, retry, note/receipt — is unchanged; only when it starts changed.

`gt mq verify`'s own exit table (distinct from `gt mq review`'s table
above — do not conflate the two commands' exit codes):

| Exit | Meaning | Refinery action |
|------|---------|-----------------|
| 0 | merge: suite passed, and no review required or review approved | proceed to merge-push |
| 1 | reject_suite: the suite failed | abort merge, FIX_NEEDED to the polecat (`Failure-Type: <tests\|build\|lint\|typecheck>`) — **any review verdict in the same result is discarded, never acted on and never sent as a second FIX_NEEDED, even if it reads as an approve** |
| 2 | escalate_review: suite passed, review infra failure (including an overlap timeout that canceled the review before it reached a verdict — an empty exit is never an approval) | skip the MR, escalate to Witness, do NOT send FIX_NEEDED |
| 3 | reject_review: suite passed, review requested changes | abort merge, FIX_NEEDED to the polecat (`Failure-Type: om-editorial`) |

Both gates stay mandatory: exit 1 always wins over whatever the review
decided, by construction — the join in internal/refinery/overlap discards a
review result whenever the suite failed, so no race between the two lets a
merge through on a partial result.
