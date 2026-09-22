p = "internal/refinery/editorial/review.go"
s = open(p).read()

T = "\t"
old = (
    T + "if !retroReview {\n"
    + T*2 + 'if note.Verdict == "approve" {\n'
    + T*3 + "if err := setEditorialReviewedHead(deps.Beads, req.MRID, head); err != nil {\n"
    + T*4 + "return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf(\"update MR bead: %v\", err), retries, tmpDir)\n"
    + T*3 + "}\n"
    + T*2 + "}\n"
    + T*2 + "return ReviewResult{Exit: 0, Note: ¬e, Retries: retries, Stderr: resultStderr}\n"
    + T + "}\n"
    + "return ReviewResult{Exit: 1, Note: ¬e, Retries: retries, Stderr: resultStderr}\n"
    + "}"
)
new = (
    T + 'if note.Verdict == "approve" {\n'
    + T*2 + "if !retroReview {\n"
    + T*3 + "// A submitted branch's push is authorized by its note, which the\n"
    + T*3 + "// refinery finds by reading editorial_reviewed_head off the MR bead.\n"
    + T*3 + "// A landed review has no such bead, and no push to precondition,\n"
    + T*3 + "// so it skips this write.\n"
    + T*3 + "if err := setEditorialReviewedHead(deps.Beads, req.MRID, head); err != nil {\n"
    + T*4 + "return failureResultWithRawOutput(deps, req, RecordFailed, fmt.Sprintf(\"update MR bead: %v\", err), retries, tmpDir)\n"
    + T*3 + "}\n"
    + T*2 + "}\n"
    + T*2 + "return ReviewResult{Exit: 0, Note: ¬e, Retries: retries, Stderr: resultStderr}\n"
    + T + "}\n"
    + "return ReviewResult{Exit: 1, Note: ¬e, Retries: retries, Stderr: resultStderr}\n"
    + "}"
)
n = s.count(old)
assert n == 1, f"old block found {n} times, expected exactly 1"
open(p, "w").write(s.replace(old, new))
print("OK: Run tail rewritten")