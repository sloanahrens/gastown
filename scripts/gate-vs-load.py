#!/usr/bin/env python3
"""gate-vs-load: the landing gate's wall time by the host load it ran under.

The landing worker prints one line per landing since gt-a025o:

    2026/10/03 20:44:11 landing_worker: [land] gt-3wjjf: stages: lint 10s, gate 28s, om 1m13s (load1 3.4)

The load suffix is what makes a slow gate readable without a sampler: the
number beside it is the 1-minute load average of the host the stages ran on.
Lines written before that (or on a host that reports no load) carry no suffix;
they are counted as "no load" and reported on their own row, never dropped.
The suffix is read wherever it sits, not only at the line's end: the
om-skipped path appends ", om skipped (overseer-reviewed)" after the stages
(gt-g46b7).

Read-only. Nothing here starts the daemon or touches its log.

Usage:
    scripts/gate-vs-load.py [--log PATH] [--since YYYY-MM-DD]
"""

import argparse
import os
import re
import statistics
import sys

DEFAULT_LOG = "~/gt/daemon/daemon.log"

# One landing's stages line. The date/time prefix is optional so a line spliced
# in without one is still read; a line with some other prefix is not a landing
# stage line and never was.
LAND_RE = re.compile(
    r"^(?:(\d{4}/\d{2}/\d{2}) \d{2}:\d{2}:\d{2}[^\n]*?)?\[land\] (\S+): stages: (.*)$"
)

# The load suffix gt-a025o added. It is not anchored to the end: the om-skipped
# path appends a status after it, so a mid-payload suffix is still the load.
# Its absence is the "no load" case.
LOAD_RE = re.compile(r" \(load1 ([0-9]+(?:\.[0-9]+)?)\)")

# A Go time.Duration.String() token: 18s, 1m45s, 5m2s, 1h1m1s. Longest unit
# first, so "45ms" never reads as "45m" + a stray "s".
DUR_RE = re.compile(r"(\d+(?:\.\d+)?)(ns|us|µs|ms|h|m|s)")
DUR_UNIT = {
    "h": 3600.0,
    "m": 60.0,
    "s": 1.0,
    "ms": 1e-3,
    "us": 1e-6,
    "µs": 1e-6,
    "ns": 1e-9,
}

# (label, upper bound); None is the last bucket, open-ended. A load on an edge
# falls in the higher bucket, so "over 45" holds 45.0 and up.
BUCKETS = [
    ("under 10", 10.0),
    ("10-20", 20.0),
    ("20-30", 30.0),
    ("30-45", 45.0),
    ("over 45", None),
]
NO_LOAD = "no load"

# The stages this report summarizes. shell and om are not gate work.
WANTED = ("gate", "lint")


def parse_go_duration(text):
    """Seconds in a Go duration string, or None when it is not one.

    go test rounds these to the second, so whole units dominate; fractional
    units still parse for a line a future change writes.
    """
    pos = 0
    total = 0.0
    found = False
    for m in DUR_RE.finditer(text):
        if m.start() != pos:
            return None  # a gap: something the unit grammar does not cover
        total += float(m.group(1)) * DUR_UNIT[m.group(2)]
        pos = m.end()
        found = True
    if not found or pos != len(text):
        return None
    return total


def format_seconds(secs):
    """A seconds count as a Go-style duration: 62.5 -> 1m2.5s, 3661 -> 1h1m1s."""
    if secs is None:
        return "-"
    v = round(secs, 1)
    parts = []
    hours = int(v // 3600)
    v = round(v - hours * 3600, 1)
    minutes = int(v // 60)
    v = round(v - minutes * 60, 1)
    if hours:
        parts.append("%dh" % hours)
    if minutes:
        parts.append("%dm" % minutes)
    if v or not parts:
        if v == int(v):
            parts.append("%ds" % int(v))
        else:
            parts.append("%ss" % ("%.1f" % v).rstrip("0").rstrip("."))
    return "".join(parts)


def bucket_of(load):
    for label, upper in BUCKETS:
        if upper is None or load < upper:
            return label
    return BUCKETS[-1][0]


def parse_line(payload):
    """(load or None, {stage: seconds}, unparsable entries) for one payload.

    A stage whose duration cannot be read is left out of the dict, and its
    whole entry text is returned in `unparsable` so the caller can count it.
    """
    load = None
    m = LOAD_RE.search(payload)
    if m:
        load = float(m.group(1))
        # Splice the suffix out where it sits, so a status appended after it
        # stays a whole ", "-separated part rather than merging with the stage
        # before it.
        payload = payload[: m.start()] + payload[m.end() :]

    stages = {}
    unparsable = []
    for part in payload.split(", "):
        part = part.strip()
        if not part:
            continue
        fields = part.split(None, 1)
        # shell and om are not this report's stages: "om skipped
        # (overseer-reviewed)" is a status, not a number to read.
        if fields[0] not in WANTED:
            continue
        if len(fields) != 2:
            unparsable.append(part)
            continue
        name, durtext = fields
        # "(retried)", "(exit N)" and "(timed out)" tails are not the number,
        # so only the token before the first space is the duration.
        secs = parse_go_duration(durtext.split(None, 1)[0])
        if secs is None:
            unparsable.append(part)
            continue
        stages[name] = secs
    return load, stages, unparsable


def read_rows(path, since):
    """Every landing stage line in `path`: (date, bucket, stages, unparsable).

    `since` is a YYYY-MM-DD string or None. Lines that are not landing stage
    lines are not counted at all; a landing stage line whose date is before
    `since` is filtered, and one with no date cannot be placed and is counted
    in `undated` when a filter is active.
    """
    rows = []
    undated = 0
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            m = LAND_RE.match(line.rstrip("\n"))
            if not m:
                continue
            date, _issue, payload = m.groups()
            if since is not None:
                if date is None:
                    undated += 1
                    continue
                if date.replace("/", "-") < since:
                    continue
            load, stages, unparsable = parse_line(payload)
            rows.append((date, NO_LOAD if load is None else bucket_of(load), stages, unparsable))
    return rows, undated


def summarize(rows):
    """One bucket -> {'landings', 'gate', 'lint'} of raw seconds lists."""
    table = {label: {"landings": 0, "gate": [], "lint": []} for label, _ in BUCKETS}
    table[NO_LOAD] = {"landings": 0, "gate": [], "lint": []}
    unparsable = 0
    for _date, bucket, stages, bad in rows:
        table[bucket]["landings"] += 1
        unparsable += len(bad)
        for name in WANTED:
            if name in stages:
                table[bucket][name].append(stages[name])
    return table, unparsable


def stat(values, fn):
    return format_seconds(fn(values)) if values else "-"


def report(path, since, rows, undated, out):
    print("gate vs load: %s" % path, file=out)
    print("since: %s" % (since if since else "all"), file=out)
    print("", file=out)

    table, unparsable = summarize(rows)
    header = "%-11s %8s %10s %10s %10s %10s" % (
        "load bucket", "landings", "gate med", "gate max", "lint med", "lint max",
    )
    print(header, file=out)
    print("-" * len(header), file=out)

    printed = 0
    for label, _upper in BUCKETS + [(NO_LOAD, None)]:
        row = table[label]
        if row["landings"] == 0:
            continue
        printed += 1
        print(
            "%-11s %8d %10s %10s %10s %10s"
            % (
                label,
                row["landings"],
                stat(row["gate"], statistics.median),
                stat(row["gate"], max),
                stat(row["lint"], statistics.median),
                stat(row["lint"], max),
            ),
            file=out,
        )
    if printed == 0:
        print("(no landings with a stages line)", file=out)

    with_load = sum(table[label]["landings"] for label, _ in BUCKETS)
    without_load = table[NO_LOAD]["landings"]
    print("", file=out)
    print(
        "totals: %d landings under load, %d with no load reading, %d unparsable stage entries"
        % (with_load, without_load, unparsable),
        file=out,
    )
    if since is not None and undated:
        print(
            "skipped: %d landing line(s) with no date, which --since cannot place"
            % undated,
            file=out,
        )
    return 0


def main(argv):
    ap = argparse.ArgumentParser(
        description="Landing gate and lint wall time by the host load it ran under."
    )
    ap.add_argument(
        "--log",
        default=DEFAULT_LOG,
        help="daemon log to read (default: %s)" % DEFAULT_LOG,
    )
    ap.add_argument(
        "--since",
        help="only landings on or after this YYYY-MM-DD (the log line's date)",
    )
    args = ap.parse_args(argv)

    if args.since is not None and not re.fullmatch(r"\d{4}-\d{2}-\d{2}", args.since):
        print("gate-vs-load: --since wants YYYY-MM-DD, got %r" % args.since, file=sys.stderr)
        return 2

    path = os.path.expanduser(args.log)
    if not os.path.exists(path):
        print("gate-vs-load: no such log file: %s" % path, file=sys.stderr)
        return 2

    rows, undated = read_rows(path, args.since)
    return report(path, args.since, rows, undated, sys.stdout)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
