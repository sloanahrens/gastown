#!/usr/bin/env python3
"""Replication check for the town rejection experiment.

Usage: replicate.py RESULT_JSON TEST_CSV [--md]

Reads the hypercubes a `mango explore` run found on the training split and
measures each one on the held-out test split: rows matched, positives, the
positive rate against the test baseline, and a two-sided exact binomial
p-value for the rate being the baseline, Holm-corrected across the boxes in
the result. A box replicates when it matches at least MIN_SIZE test rows, its
test rate is above the test baseline, and its Holm-adjusted p is below 0.05.

Columns are matched by name. A numeric range matches a value when
min <= value <= max; a category set matches when the cell text is in the set.
A cell that is empty or unparseable never matches a numeric range.

The baseline is the whole test split, box rows included, the same way the
engine's own baseline includes a cube's rows. MIN_SIZE comes from the
environment (default 30) so it is the one the experiment script used.
"""

import csv
import json
import math
import os
import sys

MIN_SIZE = int(os.environ.get("MIN_SIZE", "30"))


def binom_two_sided(k, n, p):
    """Exact two-sided binomial test: the sum of every outcome's probability
    that is at most the observed outcome's probability."""
    if n == 0:
        return 1.0
    if p <= 0:
        return 1.0 if k == 0 else 0.0
    if p >= 1:
        return 1.0 if k == n else 0.0
    log_n = math.lgamma(n + 1)
    logs = [log_n - math.lgamma(i + 1) - math.lgamma(n - i + 1)
            + i * math.log(p) + (n - i) * math.log1p(-p) for i in range(n + 1)]
    observed = logs[k]
    total = sum(math.exp(lp) for lp in logs if lp <= observed + 1e-12)
    return min(1.0, total)


def matches(cube, row):
    for r in cube["ranges"]:
        cell = row[r["column"]]
        if "categories" in r:
            if cell not in r["categories"]:
                return False
        else:
            try:
                v = float(cell)
            except (TypeError, ValueError):
                return False
            if v < r["min"] or v > r["max"]:
                return False
    return True


def holm(pvalues):
    """Holm step-down adjusted p-values, in the input order."""
    order = sorted(range(len(pvalues)), key=lambda i: pvalues[i])
    adjusted = [0.0] * len(pvalues)
    running = 0.0
    for rank, i in enumerate(order):
        running = max(running, min(1.0, (len(pvalues) - rank) * pvalues[i]))
        adjusted[i] = running
    return adjusted


def describe(cube):
    parts = []
    for r in cube["ranges"]:
        if "categories" in r:
            parts.append(f'{r["column"]} in {{{", ".join(r["categories"])}}}')
        elif r["min"] == r["max"]:
            parts.append(f'{r["column"]} = {r["min"]:g}')
        else:
            parts.append(f'{r["column"]} in [{r["min"]:g}, {r["max"]:g}]')
    return "; ".join(parts)


def main(argv):
    if len(argv) < 3:
        sys.exit(__doc__)
    result = json.load(open(argv[1]))
    rows = list(csv.DictReader(open(argv[2])))
    md = "--md" in argv
    target, positive = result["target"], str(result["positive"])
    cubes = result.get("hypercubes", [])
    if not rows:
        sys.exit(f"{argv[2]} has no data rows")
    needed = {r["column"] for c in cubes for r in c["ranges"]} | {target}
    missing = needed - set(rows[0].keys())
    if missing:
        sys.exit(f"{argv[2]} lacks columns the boxes constrain: {', '.join(sorted(missing))}")
    n_test = len(rows)
    pos_test = sum(r[target] == positive for r in rows)
    base = pos_test / n_test

    measured = []
    for cube in cubes:
        hit = [r for r in rows if matches(cube, r)]
        n = len(hit)
        k = sum(r[target] == positive for r in hit)
        measured.append((n, k, binom_two_sided(k, n, base) if n else 1.0))
    adjusted = holm([m[2] for m in measured])

    lines = []
    if md:
        lines.append(f"Test split: {n_test} rows, {pos_test} positives ({base:.1%}); "
                     f"training baseline {result['baseline']:.1%} on {result['rows']} rows.")
        lines.append("")
        lines.append("| # | box | train n | train rate | test n | test rate | test p (Holm) | replicates |")
        lines.append("|---|---|---|---|---|---|---|---|")
    replicated = 0
    for i, (cube, (n, k, _), p) in enumerate(zip(cubes, measured, adjusted), 1):
        rate = k / n if n else 0.0
        ok = n >= MIN_SIZE and rate > base and p < 0.05
        replicated += ok
        if md:
            lines.append(f"| {i} | {describe(cube)} | {cube['n']} | {cube['purity']:.1%} | {n} | "
                         f"{rate:.1%} | {p:.3f} | {'yes' if ok else 'no'} |")
        else:
            lines.append(f"{i}: n={n} k={k} rate={rate:.3f} p_holm={p:.3f} replicates={ok} :: {describe(cube)}")
    lines.append("")
    lines.append(f"{replicated} of {len(cubes)} boxes replicate on the test split "
                 f"(above the {base:.1%} baseline, Holm-adjusted p < 0.05, at least {MIN_SIZE} rows).")
    print("\n".join(lines))


if __name__ == "__main__":
    main(sys.argv)
