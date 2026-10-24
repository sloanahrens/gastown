"""Pure metric functions: rows in, rows out. No I/O, no clock."""
import datetime as dt
import math
import statistics

from .common import entropy, week_of


def percentile(sorted_values, p):
    v = sorted_values
    if len(v) == 1:
        return v[0]
    k = (len(v) - 1) * p
    lo = int(k)
    hi = min(lo + 1, len(v) - 1)
    return v[lo] + (v[hi] - v[lo]) * (k - lo)


def _mean(xs):
    return sum(xs) / len(xs) if xs else None


def _summarise(rows):
    sizes = sorted(r["lines_total"] for r in rows if r["lines_total"] is not None)
    fe = [r["file_entropy"] for r in rows if r["file_entropy"] is not None]
    pe = [r["package_entropy"] for r in rows if r["package_entropy"] is not None]
    n = len(rows)
    rej = sum(int(r["rejected_before_landing"]) for r in rows)
    return {
        "landings": n, "unreadable": sum(int(r["unreadable"]) for r in rows),
        "size_median": statistics.median(sizes) if sizes else None,
        "size_p90": percentile(sizes, 0.9) if sizes else None,
        "file_entropy_mean": _mean(fe), "package_entropy_mean": _mean(pe),
        "attempts_mean": _mean([int(r["attempts"]) for r in rows]),
        "rejected_share": rej / n, "first_try_share": 1 - rej / n,
        "manual_rescue_count": sum(int(r["manual_rescue"]) for r in rows), "thin": n < 10,
    }


def weekly_landing_metrics(landings):
    """Metrics 1, 2 and 4 per (week, rig), plus a town row (week, "ALL"). Rows hold typed values, not CSV strings."""
    groups = {}
    for r in landings:
        groups.setdefault((r["week"], r["rig"]), []).append(r)
        groups.setdefault((r["week"], "ALL"), []).append(r)
    return {k: _summarise(v) for k, v in groups.items()}


_KINDS = {"conflict", "gate", "review", "policy", "race"}


def classify_rejection(kind):
    """The kind word the landing worker writes in `MERGE REJECTION (attempt N): <kind>`; anything else is unknown."""
    k = kind.strip().lower()
    return k if k in _KINDS else "unknown"


def non_conservative(kinds):
    n = len(kinds)
    if n == 0:
        return {"rejections": 0, "conflict_gate_share": None, "unknown_share": None}
    return {"rejections": n, "conflict_gate_share": sum(k in ("conflict", "gate") for k in kinds) / n,
            "unknown_share": kinds.count("unknown") / n}


def _counts(items):
    c = {}
    for i in items:
        c[i] = c.get(i, 0) + 1
    return c.values()


def label_entropy(beads, rare):
    """Three separate figures, never blended. Labels in `rare` (fewer than 3 uses overall) fold into "other"."""
    labels = [("other" if l in rare else l) for b in beads for l in b["labels"]]
    return {"h_labels": entropy(_counts(labels)), "h_rigs": entropy(_counts([b["rig"] for b in beads])),
            "h_types": entropy(_counts([b["type"] for b in beads]))}


def dependency_density(closed):
    return sum(b["dependency_count"] for b in closed) / len(closed) if closed else None


def rho(landings, handoff_closed):
    """(non-daemon landings + handoff/crew beads closed) / daemon landings. Both parts are published."""
    daemon = sum(1 for l in landings if l["route"] == "daemon")
    num = (len(landings) - daemon) + handoff_closed
    return {"numerator": num, "denominator": daemon, "rho": num / daemon if daemon else None}


def handoff_size(closed_handoff_ids, handoff, sessions):
    sizes = [handoff[i] for i in closed_handoff_ids if i in handoff]
    sess = [sessions[i] for i in closed_handoff_ids if i in sessions]
    return {"handoff_bytes_total": sum(sizes), "handoff_bytes_median": statistics.median(sizes) if sizes else None,
            "sessions_median": statistics.median(sess) if sess else None}


def mode_shift(series, thin, weeks=None):
    """Distance between consecutive weeks of z-scored columns. The scale (sd) comes from non-thin weeks only, so a noisy
    thin week cannot inflate it. Thin, incomplete, or non-consecutive weeks (when `weeks` is given) -> None."""
    cols = sorted({k for row in series for k in row})
    sd = {}
    for c in cols:
        vals = [row[c] for row, t in zip(series, thin) if not t and row.get(c) is not None]
        sd[c] = (statistics.pstdev(vals) if len(vals) > 1 else 0.0) or 1.0
    out = [None]
    for i in range(1, len(series)):
        a, b = series[i - 1], series[i]
        gap = weeks is not None and (dt.date.fromisoformat(weeks[i]) - dt.date.fromisoformat(weeks[i - 1])).days != 7
        if gap or thin[i] or thin[i - 1] or any(a.get(c) is None or b.get(c) is None for c in cols):
            out.append(None)
            continue
        out.append(math.sqrt(sum(((b[c] - a[c]) / sd[c]) ** 2 for c in cols)))
    return out


WEEKLY_COLUMNS = ["week", "rig", "thin", "landings", "unreadable", "size_median", "size_p90", "file_entropy_mean",
                  "package_entropy_mean", "attempts_mean", "rejected_share", "first_try_share", "manual_rescue_count",
                  "h_labels", "h_rigs", "h_types", "handoff_bytes_total", "handoff_bytes_median", "sessions_median",
                  "rho_numerator", "rho_denominator", "rho", "dependency_density", "rejections", "conflict_gate_share",
                  "unknown_share", "mode_shift", "om_threshold", "dominant_model"]
_SHIFT_COLUMNS = ["size_median", "file_entropy_mean", "rejected_share"]
_EMPTY_LANDINGS = {"landings": 0, "unreadable": 0, "size_median": None, "size_p90": None, "file_entropy_mean": None,
                   "package_entropy_mean": None, "attempts_mean": None, "rejected_share": None, "first_try_share": None,
                   "manual_rescue_count": 0, "thin": True}


def _mode(values):
    """Most common value; ties go to the smallest (alphabetical for names). None when there are no values."""
    values = [v for v in values if v is not None]
    if not values:
        return None
    counts = {}
    for v in values:
        counts[v] = counts.get(v, 0) + 1
    return min(counts, key=lambda v: (-counts[v], v))


def build_weekly(landings, beads, handoff, sessions, rejection_kinds_by_week, rare_labels):
    """One row per (week, rig) and (week, "ALL"). Bead rows carry a rig name (not a prefix), like landings do."""
    by_key = weekly_landing_metrics(landings)
    land_rows, created, closed = {}, {}, {}
    for l in landings:
        for rig in (l["rig"], "ALL"):
            land_rows.setdefault((l["week"], rig), []).append(l)
    for b in beads:
        for rig in (b["rig"], "ALL"):
            if b.get("created_at"):
                created.setdefault((week_of(b["created_at"]), rig), []).append(b)
            if b.get("closed_at"):
                closed.setdefault((week_of(b["closed_at"]), rig), []).append(b)
    keys = set(by_key) | set(created) | set(closed)
    rows = []
    for key in keys:
        week, rig = key
        row = {"week": week, "rig": rig} | dict(by_key.get(key, _EMPTY_LANDINGS))
        ls = land_rows.get(key, [])
        made, done = created.get(key, []), closed.get(key, [])
        handoff_done = [b for b in done if {"handoff", "crew"} & set(b["labels"])]
        if made:
            row |= label_entropy(made, rare_labels)
        row |= handoff_size([b["id"] for b in handoff_done if "handoff" in b["labels"]], handoff, sessions)
        r = rho(ls, len(handoff_done))
        row |= {"rho_numerator": r["numerator"], "rho_denominator": r["denominator"], "rho": r["rho"],
                "dependency_density": dependency_density(done)}
        row |= non_conservative(rejection_kinds_by_week.get(key, []))
        row["om_threshold"] = _mode(l["om_threshold"] for l in ls)
        row["dominant_model"] = _mode(l["dominant_model"] for l in ls)
        rows.append(row)
    rows.sort(key=lambda r: (r["rig"], r["week"]))
    for rig in {r["rig"] for r in rows}:
        mine = [r for r in rows if r["rig"] == rig]
        shifts = mode_shift([{c: r[c] for c in _SHIFT_COLUMNS} for r in mine], [r["thin"] for r in mine],
                            [r["week"] for r in mine])
        for r, s in zip(mine, shifts):
            r["mode_shift"] = s
    for r in rows:
        for c in WEEKLY_COLUMNS:
            r.setdefault(c, None)
    return rows
