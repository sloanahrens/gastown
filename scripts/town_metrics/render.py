"""Report rendering and the 'what moved' comparison."""
import statistics


def moved_since_last(prev, cur, columns):
    """Flag a newest, non-thin week that sits more than two standard deviations from its own history.

    History is the previous run's non-thin weeks (at least 6). A flag is a prompt to look, not a verdict."""
    flags = []
    for rig in sorted({r["rig"] for r in cur}):
        newest = max(r["week"] for r in cur if r["rig"] == rig)
        latest = next((r for r in cur if r["rig"] == rig and r["week"] == newest), None)
        if latest is None or latest["thin"]:
            continue
        for col in columns:
            hist = [r[col] for r in prev if r["rig"] == rig and not r["thin"] and r.get(col) is not None]
            if len(hist) < 6 or latest.get(col) is None:
                continue
            mu, sd = statistics.fmean(hist), statistics.pstdev(hist)
            if sd > 0 and abs(latest[col] - mu) > 2 * sd:
                flags.append({"rig": rig, "week": newest, "column": col, "value": latest[col], "mean": mu, "sd": sd})
    return flags


HEADLINE_COLUMNS = ["size_median", "file_entropy_mean", "rejected_share", "rho", "mode_shift"]

# (title, definition, columns, caveat): fixed text so hm chunks the report predictably.
METRIC_SECTIONS = [
    ("Landing size",
     "Lines added plus deleted per landing: median and 90th percentile, with landings per week.",
     ["landings", "unreadable", "size_median", "size_p90"],
     "Landings with an unreadable commit are counted in `unreadable`, never dropped silently."),
    ("File and package entropy",
     "Shannon entropy, in bits, of lines changed across files and across directories, averaged over the week's landings.",
     ["file_entropy_mean", "package_entropy_mean"],
     "0 for a single-file landing. A rename counts as one file."),
    ("Mode shift",
     "Distance between consecutive weeks of the z-scored median size, file entropy and rework share.",
     ["mode_shift"],
     "Thin weeks (fewer than 10 landings) are reported but never flagged."),
    ("Rework depth",
     "Attempts per landed bead, share rejected before landing, and share approved first try.",
     ["attempts_mean", "rejected_share", "first_try_share"],
     "Beads abandoned before landing are absent; the denominator is landed beads."),
    ("Label entropy",
     "Entropy, in bits, of the labels, rigs and issue types of beads created that week.",
     ["h_labels", "h_rigs", "h_types"],
     "Three separate figures, not blended. Labels with fewer than 3 uses fold into other."),
    ("Handoff size",
     "Bytes of notes plus comments on handoff-labelled beads closed that week, and sessions per bead.",
     ["handoff_bytes_total", "handoff_bytes_median", "sessions_median"],
     "Includes beads in `~/.claude`."),
    ("rho",
     "(Non-daemon landings plus crew or handoff beads closed) divided by daemon-dispatched landings.",
     ["rho_numerator", "rho_denominator", "rho"],
     "Numerator and denominator are both published. The proxy for human-planned work is crew and handoff beads."),
    ("Dependency density",
     "Mean outgoing dependency count of beads closed that week.",
     ["dependency_density"],
     "Outgoing edges per bead; `bd list` gives counts, not edges."),
    ("Non-conservative merges",
     "Rejections of kind conflict or gate as a share of all rejections, and the count of landings that were not daemon-routed.",
     ["rejections", "conflict_gate_share", "unknown_share", "manual_rescue_count"],
     "Kinds come from the kind word in the bead's rejection notes; an unrecognised word counts as `unknown`. Only beads that eventually landed are counted, in the week they landed."),
]


def _fmt(x):
    if x is None:
        return "n/a"
    if isinstance(x, float):
        return format(x, ".3g")
    return str(x)


def _mean(values):
    values = [v for v in values if v is not None]
    return statistics.fmean(values) if values else None


def _table(header, rows):
    out = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    out += ["| " + " | ".join(r) + " |" for r in rows]
    return "\n".join(out)


def render_report(date, weekly, flags, footer, mango_section, regime_events, backtest="Not run."):
    town = sorted((r for r in weekly if r["rig"] == "ALL"), key=lambda r: r["week"])
    blocks = ["# Town metrics " + date]

    last, before = town[-4:], town[-8:-4]
    head = [[c, _fmt(_mean(r[c] for r in last)), _fmt(_mean(r[c] for r in before))] for c in HEADLINE_COLUMNS]
    blocks.append("## Headline\n\nLast four weeks against the four before (town, all rigs).\n\n"
                  + _table(["Metric", "Last 4 weeks", "Previous 4 weeks"], head))

    if flags:
        lines = ["- %s %s %s: %s (history mean %s, sd %s)" % (f["rig"], f["week"], f["column"], _fmt(f["value"]),
                                                              _fmt(f["mean"]), _fmt(f["sd"])) for f in flags]
    else:
        lines = ["Nothing moved more than two standard deviations."]
    blocks.append("## Moved since last run\n\n" + "\n".join(lines))

    for n, (title, definition, cols, caveat) in enumerate(METRIC_SECTIONS, 1):
        rows = [[r["week"] + (" (thin)" if r["thin"] else "")] + [_fmt(r[c]) for c in cols] for r in town]
        blocks.append("## Metric %d: %s\n\n%s\n\n%s\n\nCaveat: %s"
                      % (n, title, definition, _table(["Week"] + cols, rows), caveat))

    blocks.append("## Regime events\n\n" + "\n".join("- %s: %s" % (e["date"], e["what"]) for e in regime_events))
    blocks.append("## Usefulness backtest\n\n" + backtest)
    blocks.append("## Mango findings\n\n" + mango_section)

    read = "\n".join("- %s: %s" % (k, v) for k, v in sorted(footer["rows_read"].items()))
    dropped, gaps = footer["rows_dropped"], footer.get("gaps", [])
    quality = ["## Data quality", "Rows read:\n" + read if read else "Rows read: none",
               "Rows dropped: %d" % len(dropped) + "".join("\n- %s: %s" % d for d in dropped),
               "Other gaps: %d" % len(gaps) + "".join("\n- %s: %s" % g for g in gaps),
               "Sources unreadable: " + (", ".join(footer["sources_unreadable"]) or "none")]
    blocks.append("\n\n".join(quality))
    return "\n\n".join(blocks) + "\n"
