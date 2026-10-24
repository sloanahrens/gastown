"""Shared helpers: week bucketing, entropy, CSV I/O, a drop log. No other module's concerns live here."""
import csv
import datetime as dt
import math


def parse_ts(ts):
    return dt.datetime.fromisoformat(ts.replace("Z", "+00:00"))


def week_of(ts):
    d = parse_ts(ts).date()
    return (d - dt.timedelta(days=d.weekday())).isoformat()


def entropy(weights):
    w = [x for x in weights if x > 0]
    total = sum(w)
    if len(w) <= 1:
        return 0.0
    return -sum((x / total) * math.log2(x / total) for x in w)


def write_csv(path, rows, columns):
    with open(path, "w", newline="") as f:
        out = csv.writer(f)
        out.writerow(columns)
        for r in rows:
            out.writerow(["" if r.get(c) is None else r.get(c) for c in columns])


def read_csv(path):
    with open(path, newline="") as f:
        return list(csv.DictReader(f))


class Dropped:
    """Every row a loader refuses is recorded here so the row-count assertion and the footer can name it."""

    def __init__(self):
        self.items = []
        self.failed = []   # sources that could not be read at all (structured, never matched by message text)

    def add(self, source, reason):
        self.items.append((source, reason))

    def fail(self, source, reason):
        self.failed.append(source)
        self.add(source, reason)

    @property
    def count(self):
        return len(self.items)
