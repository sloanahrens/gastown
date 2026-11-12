<!-- Copied from docs/birds/daily-request.md, the source. Edit that file, copy it here, and put this line back: Request cuts it before the request is printed, and TestEmbeddedRequestIsTheDocsCopy fails if the two texts differ. -->

<NAME>: it is time for your daily report. Write it, publish the two files the dashboard reads, and tell the overseer it is in.

Write the report in these eight sections, in this order:

1. Today — the day in one line.
2. Landed — what shipped, with bead ids and commit hashes.
3. In flight — what is open, what you are doing next on it, and how you will know it is done.
4. Blocked — what cannot move without someone else, and who that is.
5. Decisions needed — every decision the operator must make, each with your recommendation.
6. Risks — what breaks or drifts if nobody looks, and the first sign it is happening.
7. Filed — beads filed, closed, rejected or reassigned since your last report.
8. Next — what you will do next, in the order you will do it.

Write the page as HTML to daily.html, build the sidecar with jq, and pipe both to the publisher, which writes them under <RUNTIME_DIR>/reports/<NAME>/:

  jq -n --arg bird "<NAME>" --arg written_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg title "<the report's headline>" --arg summary "<the report in three sentences>" \
    '{bird:$bird,written_at:$written_at,title:$title,summary:$summary,needs_operator:false,decisions:0}' > sidecar.json
  jq -n --rawfile page daily.html --slurpfile sidecar sidecar.json \
    '{bird:"<NAME>",page:$page,sidecar:$sidecar[0]}' | kubectl --context colima-gt-control -n gt exec -i "$(kubectl --context colima-gt-control -n gt get pods -l app.kubernetes.io/name=daemon --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')" -c daemon -- gt report publish --bird <NAME>

Run the publish command in the daemon pod, not with the Mac's gt: the Mac's gt writes under the Mac's .runtime, and the daemon and the dashboard read reports from the pod's .runtime, so a report the Mac's gt publishes never reaches them. The -i sends the document to the pod's stdin; without it the pod receives an empty document and writes nothing. Once the gt-yjzepk wrapper is installed on the Mac, return to plain gt report publish --bird <NAME> in the pipeline above, and drop the kubectl part.

The publisher writes <RUNTIME_DIR>/reports/<NAME>/daily.html and daily.json, keeps the page it replaced as daily.prev.html, and prints the two paths and their sizes. It refuses a page over 1 MB, a sidecar over 16 KiB or one the reader would not accept, and names the field it refused: fix that field and pipe the document again.

The sidecar's fields, exactly:
  {"bird":"<NAME>","written_at":"<RFC 3339 instant>","title":"<the report's headline>","summary":"<the report in three sentences>","needs_operator":false,"decisions":0}
  - bird is your name, and must match --bird and the report directory.
  - written_at is when you finished writing, in RFC 3339; the dashboard marks the report stale 26 hours after it.
  - title is the report's headline, short enough for one row.
  - summary is the report in three sentences, for the dashboard row.
  - needs_operator is true when section 5 holds a decision waiting on the operator.
  - decisions is how many decisions section 5 records.

Send the overseer one line when the publisher has written both files, saying whether needs_operator is true.
