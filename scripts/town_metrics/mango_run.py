"""Mango step: split by date, bin with train-only cuts, drop post-hoc columns, run the CLI, check boxes on the holdout.

Mirrors the first experiment's ingest/explore/report calls and reuses the replication maths of
town_metrics/replicate.py. The mango binary is injected through `runner`, so unit tests use a fake."""
import importlib.util
import json
import os
import shutil

from . import common, tables

TARGETS = ["rejected_before_landing", "manual_rescue", "reached_overrun_stop"]
_IDENTITY = {"bead", "landed_at", "week"}
_DUPLICATE = {"model"}                       # same value as dominant_model at landing level
_LEAKS = {"manual_rescue": {"route"}}        # route defines this target: a box on it would only restate it
REGIME = {"om_threshold", "dominant_model"}


class MangoError(Exception):
    pass


def split_by_date(rows, train_fraction=0.7):
    ordered = sorted(rows, key=lambda r: r["landed_at"])
    k = int(len(ordered) * train_fraction + 1e-9)
    return ordered[:k], ordered[k:]


def quartile_cuts(rows, column):
    xs = sorted(float(r[column]) for r in rows if r[column] not in ("", None))
    return [xs[len(xs) * i // 4] for i in (1, 2, 3)] if len(xs) >= 4 else []


def bin_value(v, cuts):
    if v in ("", None):
        return "none"
    return "q%d" % (1 + sum(float(v) >= c for c in cuts))


def prepare(rows, explain_only, target, binned=()):
    train, holdout = split_by_date(rows)
    drop = set(explain_only) | set(TARGETS) | _IDENTITY | _DUPLICATE | _LEAKS.get(target, set())
    predictors = [c for c in tables.LANDING_COLUMNS if c not in drop]
    train, holdout = [dict(r) for r in train], [dict(r) for r in holdout]
    for col in binned:
        cuts = quartile_cuts(train, col)  # cut points from the training split only
        for r in train + holdout:
            r[col] = bin_value(r[col], cuts)
    return {"train": train, "holdout": holdout, "predictors": predictors, "columns": predictors + [target],
            "train_positives": sum(r[target] == "1" for r in train),
            "holdout_positives": sum(r[target] == "1" for r in holdout),
            "last_positive_at": max((r["landed_at"] for r in train + holdout if r[target] == "1"), default=None)}


def write_split(prepared, run_dir):
    import csv
    os.makedirs(run_dir, exist_ok=True)
    for name in ("train", "holdout"):
        path = os.path.join(run_dir, "train.csv" if name == "train" else "test.csv")
        with open(path, "w", newline="") as f:
            w = csv.DictWriter(f, fieldnames=prepared["columns"], extrasaction="ignore")
            w.writeheader()
            w.writerows(prepared[name])


def find_binary(name="mango", path=None):
    return shutil.which(name, path=path)


def run_mango(binary, run_dir, target, runner, min_support=30, seed=20261019, min_purity=0.15, permutations=1000, widen=True):
    """ingest -> explore -> report on run_dir/train.csv. runner(argv) -> (returncode, stdout)."""
    def call(*args):
        rc, out = runner([binary, *args])
        if rc != 0:
            raise MangoError("mango %s failed (exit %d): %s" % (args[0], rc, out[-800:]))
        return out
    parquet, result_json = os.path.join(run_dir, "train.parquet"), os.path.join(run_dir, "result.json")
    call("ingest", os.path.join(run_dir, "train.csv"), "--out", parquet)
    # --widen: small wide tables otherwise yield no cube of min-size rows (ma-65b, ma-2g6)
    call("explore", parquet, "--target", target, "--positive", "1", "--seed", str(seed), "--permutations",
         str(permutations), "--min-size", str(min_support), "--min-purity", str(min_purity),
         *(["--widen"] if widen else []), "--out", result_json)
    report_md = call("report", result_json, "--md")
    with open(result_json) as f:
        result = json.load(f)
    stats = result.get("stats") or {}
    gen = stats.get("generate") or {}
    return {"iterations": gen.get("iterations"), "germs": gen.get("germsTried"), "candidates": stats.get("candidates"),
            "generated": gen.get("generated"), "valid": gen.get("valid"), "significant": gen.get("significant"),
            "skipped_columns": [(c["column"], c["reason"]) for c in gen.get("skippedColumns") or []],
            "boxes": result.get("hypercubes") or [], "report_md": report_md, "result": result}


def _replicate_module():
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "replicate.py")
    spec = importlib.util.spec_from_file_location("town_rejection_replicate", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _mean(rows, col):
    xs = [float(r[col]) for r in rows if r.get(col) not in ("", None)]
    return sum(xs) / len(xs) if xs else None


def check_holdout(boxes, rows, target, min_support, explain):
    """Each box measured on the holdout: n, k, rate against the holdout baseline, Holm p, replicates, label, and the
    post-hoc columns' means inside the box against the whole holdout (explain only, never predictors).

    The p-value is the two-sided exact binomial test of the first experiment (Holm-corrected across the boxes), and a
    box must also sit ABOVE the baseline to replicate; that is two-sided evidence filtered by direction, not a one-sided
    test. A box significantly below the baseline is reported as "no", not flagged: boxes are searched for elevated rates."""
    rep = _replicate_module()
    base = sum(r[target] == "1" for r in rows) / len(rows)
    measured = []
    for box in boxes:
        hit = [r for r in rows if rep.matches(box, r)]
        n, k = len(hit), sum(r[target] == "1" for r in hit)
        measured.append((box, hit, n, k, rep.binom_two_sided(k, n, base) if n else 1.0))
    adjusted = rep.holm([m[4] for m in measured])
    out = []
    for (box, hit, n, k, _), p in zip(measured, adjusted):
        rate = k / n if n else 0.0
        cols = {r["column"] for r in box["ranges"]}
        out.append({"box": rep.describe(box), "train_n": box.get("n"), "train_rate": box.get("purity"),
                    "n": n, "k": k, "rate": rate, "baseline": base, "p_holm": p,
                    "replicates": n >= min_support and rate > base and p < 0.05,
                    "label": "period" if cols and cols <= REGIME else "",
                    "explain": {c: (_mean(hit, c), _mean(rows, c)) for c in explain}})
    return out


_EXPLAIN_NUMERIC = ["lines_total", "files_touched", "file_entropy", "package_entropy", "attempts"]


def _fmt(x, pct=False):
    if x is None:
        return "n/a"
    return ("%.1f%%" % (100 * x)) if pct else ("%.3g" % x if isinstance(x, float) else str(x))


def _head(target, prep):
    last = prep["last_positive_at"]
    lines = ["### %s" % target,
             "Train: %d rows, %d positive. Holdout: %d rows, %d positive." % (
                 len(prep["train"]), prep["train_positives"], len(prep["holdout"]), prep["holdout_positives"])]
    if last:
        lines.append("Last positive landed %s." % last[:10])
    return lines


def _target_part(target, prep, out, checks):
    lines = _head(target, prep)
    lines.append("Search: %s cubes generated, %s valid, %s significant, %s tested (%s iterations, %s germs tried)."
                 % (out["generated"], out["valid"], out["significant"], out["candidates"], out["iterations"],
                    out["germs"]))
    if out["skipped_columns"]:
        lines.append("Columns no box constrained: " + "; ".join("%s (%s)" % c for c in out["skipped_columns"]) + ".")
    if not out["boxes"]:
        lines.append("No boxes: %s cubes generated, %s valid. When valid is 0 the generator never reached the minimum "
                     "support, which is not evidence that nothing is predictive." % (out["generated"], out["valid"]))
        return "\n\n".join(lines)
    if prep["holdout_positives"] == 0:
        lines.append("Holdout has no positives, so the %d box(es) found on train cannot be checked on this split. "
                     "The outcome stopped before the holdout period: that is a regime change in itself, not a result."
                     % len(out["boxes"]))
        return "\n\n".join(lines)
    cols = sorted({c for ch in checks for c in ch["explain"]})
    header = ["#", "box", "label", "train n", "train rate", "holdout n", "holdout rate", "baseline", "p (Holm)",
              "replicates"] + ["%s (box vs all)" % c for c in cols]
    rows = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    for i, ch in enumerate(checks, 1):
        cells = [str(i), ch["box"], ch["label"] or "-", _fmt(ch["train_n"]), _fmt(ch["train_rate"], True), str(ch["n"]),
                 _fmt(ch["rate"], True), _fmt(ch["baseline"], True), "%.3f" % ch["p_holm"],
                 "yes" if ch["replicates"] else "no"]
        cells += ["%s vs %s" % (_fmt(ch["explain"][c][0]), _fmt(ch["explain"][c][1])) for c in cols]
        rows.append("| " + " | ".join(cells) + " |")
    lines.append("\n".join(rows))
    return "\n\n".join(lines)


def mango_section(out_dir, binary, runner, min_support=30, min_positives=30):
    """The report's "Mango findings" text. Runs profile on weekly.csv and explore per target on landings.csv."""
    if binary is None:
        return "mango binary not found; CSVs written to %s, mango step skipped." % out_dir
    rows = common.read_csv(os.path.join(out_dir, "landings.csv"))
    with open(os.path.join(out_dir, "landings.sidecar.json")) as f:
        explain_only = json.load(f)["explain_only"]
    parts, summary = [], {}
    rc, prof = runner([binary, "profile", os.path.join(out_dir, "weekly.csv")])
    parts.append("### Weekly table profile\n\n" + ("```\n%s\n```" % prof.rstrip().replace(out_dir, "<run>") if rc == 0
                                                     else "mango profile failed (exit %d): %s" % (rc, prof[-400:])))
    for target in TARGETS:
        prep = prepare(rows, explain_only, target)
        if prep["train_positives"] < min_positives:
            parts.append("\n\n".join(_head(target, prep) + ["Skipped: fewer than %d positives in train." % min_positives]))
            continue
        run_dir = os.path.join(out_dir, "mango", target)
        write_split(prep, run_dir)
        try:
            out = run_mango(binary, run_dir, target, runner, min_support=min_support)
        except MangoError as e:
            parts.append("\n\n".join(_head(target, prep) + [str(e)]))
            continue
        checks = (check_holdout(out["boxes"], prep["holdout"], target, min_support, _EXPLAIN_NUMERIC)
                  if out["boxes"] and prep["holdout_positives"] else [])
        summary[target] = {"boxes": len(out["boxes"]), "replicating": sum(c["replicates"] for c in checks),
                           "holdout_positives": prep["holdout_positives"]}
        parts.append(_target_part(target, prep, out, checks))
    with open(os.path.join(out_dir, "mango-summary.json"), "w") as f:
        json.dump(summary, f)
    return "\n\n".join(parts)
