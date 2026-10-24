"""Usefulness backtest: does a metric move at a known regime event, without being told when?

A metric stays if it moves at an event, or a mango box built on it replicates, or Sloan would act on it. Pure functions;
the caller supplies typed weekly rows, the event list, mango's replication results and Sloan's answers."""
import datetime as dt
import statistics

from .common import week_of

# metric name -> weekly columns that carry it
METRICS = {
    "landing size": ["size_median", "size_p90"],
    "file and package entropy": ["file_entropy_mean", "package_entropy_mean"],
    "mode shift": ["mode_shift"],
    "rework depth": ["attempts_mean", "rejected_share"],
    "label entropy": ["h_labels", "h_rigs", "h_types"],
    "handoff size": ["handoff_bytes_total", "handoff_bytes_median"],
    "rho": ["rho"],
    "dependency density": ["dependency_density"],
    "non-conservative merges": ["conflict_gate_share"],
}
# These are computed from landings, so a thin week (fewer than 10 landings) is noise for them. Bead-derived columns are not.
LANDING_DERIVED = {"size_median", "size_p90", "file_entropy_mean", "package_entropy_mean", "attempts_mean", "rejected_share",
                   "first_try_share", "manual_rescue_count", "rejections", "conflict_gate_share", "unknown_share",
                   "mode_shift", "rho", "rho_numerator", "rho_denominator"}


def series(weekly_all, column):
    rows = sorted((r for r in weekly_all if r["rig"] == "ALL"), key=lambda r: r["week"])
    return [(r["week"], r[column]) for r in rows
            if r.get(column) is not None and not (column in LANDING_DERIVED and r["thin"])]


def _days(a, b):
    return (dt.date.fromisoformat(b) - dt.date.fromisoformat(a)).days


def moves_at_event(weekly_all, column, event_date, min_changes=5):
    """True/False when the change into the event week is/isn't beyond two standard deviations of the other week-to-week
    changes; None when the history is too short or too flat to say (never a silent False)."""
    s = series(weekly_all, column)
    event_week = week_of(event_date + "T00:00:00Z")
    changes = {w: v - pv for (pw, pv), (w, v) in zip(s, s[1:]) if _days(pw, w) == 7}
    if event_week not in changes or len(changes) < min_changes:
        return None
    others = [c for w, c in changes.items() if w != event_week]
    sd = statistics.pstdev(others) if len(others) > 1 else 0.0
    if sd == 0:
        return None
    return abs(changes[event_week]) > 2 * sd


def chance_of_coincidence(weekly_all, column, events, min_changes=5):
    """Chance that the series' single largest move falls in one of the event weeks by luck: event weeks / changes."""
    s = series(weekly_all, column)
    weeks = [w for (pw, _), (w, _) in zip(s, s[1:]) if _days(pw, w) == 7]
    if len(weeks) < min_changes:
        return None
    event_weeks = {week_of(e["date"] + "T00:00:00Z") for e in events}
    return len(event_weeks & set(weeks)) / len(weeks)


def _pearson(xs, ys):
    mx, my = statistics.fmean(xs), statistics.fmean(ys)
    sx = sum((x - mx) ** 2 for x in xs) ** 0.5
    sy = sum((y - my) ** 2 for y in ys) ** 0.5
    if sx == 0 or sy == 0:
        return None
    return sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / (sx * sy)


def correlated_pairs(weekly, columns, threshold=0.9, min_rows=6):
    """Column pairs with |r| above the threshold over the (week, rig) rows where both are present and not thin."""
    out = []
    for i, a in enumerate(columns):
        for b in columns[i + 1:]:
            shared = [r for r in weekly if r.get(a) is not None and r.get(b) is not None and not r["thin"]]
            if len(shared) < min_rows:
                continue
            r = _pearson([x[a] for x in shared], [x[b] for x in shared])
            if r is not None and abs(r) > threshold:
                out.append((a, b, r))
    return out


def verdict(moved, replicated, sloan_would_act):
    """keep if any evidence holds; drop only when every source has answered no; otherwise undecided."""
    if moved is True or replicated is True or sloan_would_act is True:
        return "keep"
    if moved is False and replicated is False and sloan_would_act is False:
        return "drop"
    return "undecided"


WEAK_ABOVE = 0.25  # chance of coincidence above this: a move at an event does not count as evidence


def _cell(x):
    return "n/a" if x is None else ("yes" if x is True else "no" if x is False else str(x))


def backtest_section(weekly, events, replicated, sloan):
    """Markdown for the report. replicated and sloan map a metric name to True/False/None."""
    all_rows = [r for r in weekly if r["rig"] == "ALL"]
    lines = ["Events (from `docs/metrics/regime-events.json`): "
             + "; ".join("%s %s%s" % (e["date"], e["what"], "" if e.get("confirmed") else " [unconfirmed]")
                         for e in events) + ".", ""]
    header = ["Metric", "moved at event", "chance of coincidence", "mango box replicates", "Sloan would act", "verdict"]
    table = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    short = False
    for name, cols in METRICS.items():
        answers = [moves_at_event(all_rows, c, e["date"]) for c in cols for e in events]
        if any(a is True for a in answers):
            moved = True
        elif answers and all(a is False for a in answers):
            moved = False
        else:
            moved = None
        short = short or moved is None
        chances = [chance_of_coincidence(all_rows, c, events) for c in cols]
        chances = [c for c in chances if c is not None]
        worst = max(chances) if chances else None
        weak = moved is True and worst is not None and worst > WEAK_ABOVE
        shown = "yes (weak)" if weak else _cell(moved)
        # a move that luck explains that often is a prompt to look, not evidence for keeping the metric
        counted = None if weak else moved
        table.append("| %s | %s | %s | %s | %s | %s |" % (
            name, shown, "%.0f%%" % (100 * worst) if worst is not None else "n/a", _cell(replicated.get(name)),
            _cell(sloan.get(name)), verdict(counted, replicated.get(name), sloan.get(name))))
    lines += table
    lines += ["", "A yes under moved-at-event is weak evidence when the chance of coincidence is high: with few weekly "
                  "changes and several events, the series' biggest move lands on an event week by luck that often. "
                  "Read a verdict as a prompt to look, not as a result."]
    if short:
        lines += ["", "n/a under \"moved at event\" means insufficient history: fewer than 5 comparable week-to-week "
                      "changes, an event outside the series, or a flat history. It is not a no."]
    cols = [c for cs in METRICS.values() for c in cs]
    pairs = correlated_pairs(weekly, cols)
    lines += ["", "Pairs correlated above 0.9 (merged into one metric): "
              + ("; ".join("%s ~ %s (r=%.2f)" % p for p in pairs) if pairs else "none") + "."]
    return "\n".join(lines)


# mango target -> the metric a box on it speaks for (post-hoc columns only explain boxes; the target itself is the metric)
TARGET_METRIC = {"rejected_before_landing": "rework depth", "manual_rescue": "non-conservative merges"}


def replicated_from_summary(summary):
    """True/False per metric when its mango target had boxes and a holdout with positives to check them on; absent otherwise."""
    out = {}
    for target, metric in TARGET_METRIC.items():
        s = summary.get(target)
        if s and s["boxes"] and s["holdout_positives"]:
            out[metric] = s["replicating"] > 0
    return out
