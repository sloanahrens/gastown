<!-- Copied from docs/birds/daily-request.md, the source. Edit that file, copy it here, and put this line back: Request cuts it before the request is printed, and TestEmbeddedRequestIsTheDocsCopy fails if the two texts differ. -->

<NAME>: it is time for your daily report. Write it, write the two files the dashboard reads, and tell the overseer it is in.

Write the report in these eight sections, in this order:

1. Today — the day in one line.
2. Landed — what shipped, with bead ids and commit hashes.
3. In flight — what is open, what you are doing next on it, and how you will know it is done.
4. Blocked — what cannot move without someone else, and who that is.
5. Decisions needed — every decision the operator must make, each with your recommendation.
6. Risks — what breaks or drifts if nobody looks, and the first sign it is happening.
7. Filed — beads filed, closed, rejected or reassigned since your last report.
8. Next — what you will do next, in the order you will do it.

Write the two files the dashboard's Reports page reads:

- Write the page to <RUNTIME_DIR>/<NAME>/reports/daily.html and the sidecar <RUNTIME_DIR>/<NAME>/reports/daily.json beside it, with exactly these fields:
  {"bird":"<NAME>","written_at":"<RFC 3339 instant>","title":"<the report's headline>","summary":"<the report in three sentences>","needs_operator":false,"decisions":0}
  - bird is your name, and must match the directory the files are in.
  - written_at is when you finished writing, in RFC 3339; the dashboard marks the report stale 26 hours after it.
  - title is the report's headline, short enough for one row.
  - summary is the report in three sentences, for the dashboard row.
  - needs_operator is true when section 5 holds a decision waiting on the operator.
  - decisions is how many decisions section 5 records.

Send the overseer one line when both files are written, saying whether needs_operator is true.
