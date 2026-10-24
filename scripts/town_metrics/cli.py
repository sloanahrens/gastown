"""extract and report: wire loaders -> tables -> metrics -> CSVs and the Markdown report.

`extract` and `report` take an Env of injected sources, so unit tests never touch ~/gt, bd or git.
The real Env is built in `real_env` (impure wiring only, kept thin).

This tool is this town's by design: the rig list and the bead prefixes (RIGS, PREFIX_TO_RIG) are constants below, and
the town root and docs directory come from TOWN_METRICS_TOWN and TOWN_METRICS_DOCS."""
import dataclasses
import json
import os

from . import backtest, common, loaders, mango_run, metrics, render, snapshot, tables

BEAD_COLUMNS = ["id", "rig", "created_at", "closed_at", "type", "labels", "dependency_count", "comment_count",
                "handoff_bytes", "sessions"]
_NUMERIC = {"landings", "unreadable", "size_median", "size_p90", "file_entropy_mean", "package_entropy_mean",
            "attempts_mean", "rejected_share", "first_try_share", "manual_rescue_count", "h_labels", "h_rigs", "h_types",
            "handoff_bytes_total", "handoff_bytes_median", "sessions_median", "rho_numerator", "rho_denominator", "rho",
            "dependency_density", "rejections", "conflict_gate_share", "unknown_share", "mode_shift", "om_threshold"}
_FLAGGED = ["size_median", "file_entropy_mean", "rejected_share", "attempts_mean", "rho", "dependency_density",
            "h_labels", "handoff_bytes_median", "conflict_gate_share"]


class RowCountError(Exception):
    pass


@dataclasses.dataclass
class Env:
    ledger_lines: dict          # rig -> list of jsonl lines
    review_lines: list
    diff_source: object         # .stats(rig, commit) -> dict | None
    run_bd: object              # (cwd, args) -> stdout | None
    bead_dirs: list
    prefix_to_rig: dict
    usage_files: dict           # bead -> parsed *.work.json
    overrun_states: dict        # bead -> parsed overrun-guard state file
    om_commits: dict            # rig -> [(iso time, .om.json text)] oldest first
    out_dir: str
    docs_dir: str
    today: str
    unreadable: list = dataclasses.field(default_factory=list)   # sources the wiring could not open at all
    handoff_cache: dict = dataclasses.field(default_factory=dict)
    bd_show_cap: int = 150


def _nonblank(lines):
    return sum(1 for l in lines if l.strip())


def extract(env, ledger_loader=loaders.load_ledger, review_loader=loaders.load_reviews):
    os.makedirs(env.out_dir, exist_ok=True)
    dropped = common.Dropped()

    ledger = ledger_loader(env.ledger_lines, dropped)
    ledger_lines = sum(_nonblank(v) for v in env.ledger_lines.values())
    ledger_drops = sum(1 for src, _ in dropped.items if src.startswith("ledger:"))
    if len(ledger) + ledger_drops != ledger_lines:
        raise RowCountError("ledger: %d rows + %d logged drops != %d source lines"
                            % (len(ledger), ledger_drops, ledger_lines))

    reviews = review_loader(env.review_lines, dropped)
    review_drops = sum(1 for src, _ in dropped.items if src == "reviews")
    if len(reviews) + review_drops != _nonblank(env.review_lines):
        raise RowCountError("reviews: %d rows + %d logged drops != %d source lines"
                            % (len(reviews), review_drops, _nonblank(env.review_lines)))
    beads = loaders.load_beads(env.run_bd, env.bead_dirs, dropped)
    for b in beads:
        b["rig"] = env.prefix_to_rig.get(b["rig"], b["rig"])
    unreadable = list(env.unreadable) + dropped.failed
    bd_lists_ok = len([s for s in dropped.failed if s.startswith("bd:")]) < len(env.bead_dirs)

    landed_at = {l["bead"]: l["landed_at"] for l in ledger}
    note_kinds = {b["id"]: b["rejection_kinds"] for b in beads if b["rejection_kinds"] and b["id"] in landed_at}
    rework = loaders.rework_by_bead(reviews, landed_at, note_kinds)
    usage = loaders.load_usage(env.usage_files)
    overrun = loaders.load_overrun(env.overrun_states, landed_at)
    eras = {rig: loaders.om_eras(c) for rig, c in env.om_commits.items()}
    beads_by_id = {b["id"]: b for b in beads}
    shaped = {b["id"] for b in beads if b["shaped"]}
    landings = tables.build_landings(ledger, rework, env.diff_source, beads_by_id, usage, overrun, eras, shaped, dropped)

    handoff_dirs = {b["id"]: b["dir"] for b in beads if "handoff" in b["labels"]}
    handoff = loaders.handoff_bytes(env.run_bd, handoff_dirs, dropped, env.bd_show_cap, env.handoff_cache)
    sessions = {bead: u["sessions"] for bead, u in usage.items()}
    kinds = {}
    for l in landings:
        for kind in note_kinds.get(l["bead"], []):
            for rig in (l["rig"], "ALL"):
                kinds.setdefault((l["week"], rig), []).append(metrics.classify_rejection(kind))

    use = {}
    for b in beads:
        for lab in set(b["labels"]):
            use[lab] = use.get(lab, 0) + 1
    rare = {lab for lab, n in use.items() if n < 3}
    weekly = metrics.build_weekly(landings, beads, handoff, sessions, kinds, rare)

    common.write_csv(os.path.join(env.out_dir, "landings.csv"), landings, tables.LANDING_COLUMNS)
    tables.write_landing_sidecar(os.path.join(env.out_dir, "landings.sidecar.json"))
    common.write_csv(os.path.join(env.out_dir, "weekly.csv"), weekly, metrics.WEEKLY_COLUMNS)
    if bd_lists_ok:  # every bd list failed -> no beads.csv: absence is the signal, an empty table is a real answer
        rows = [b | {"labels": ";".join(b["labels"]), "handoff_bytes": handoff.get(b["id"]),
                     "sessions": (usage.get(b["id"]) or {}).get("sessions")} for b in beads]
        common.write_csv(os.path.join(env.out_dir, "beads.csv"), rows, BEAD_COLUMNS)

    footer = {"rows_read": {"ledger": ledger_lines, "reviews": _nonblank(env.review_lines), "beads": len(beads),
                            "usage": len(env.usage_files)},
              "rows_dropped": [d for d in dropped.items if d[0].startswith("ledger:") or d[0] == "reviews"],
              "gaps": [d for d in dropped.items if not (d[0].startswith("ledger:") or d[0] == "reviews")],
              "sources_unreadable": unreadable}
    with open(os.path.join(env.out_dir, "footer.json"), "w") as f:
        json.dump(footer, f)
    return {"landings": landings, "weekly": weekly, "footer": footer}


def load_result(out_dir):
    """What `report` needs from an earlier extract: the typed weekly rows and the data-quality footer."""
    with open(os.path.join(out_dir, "footer.json")) as f:
        footer = json.load(f)
    footer["rows_dropped"] = [tuple(d) for d in footer["rows_dropped"]]
    return {"weekly": typed_weekly(common.read_csv(os.path.join(out_dir, "weekly.csv"))), "footer": footer}


def typed_weekly(rows):
    """CSV strings back to the typed rows the metric and render code expects."""
    out = []
    for r in rows:
        t = {"week": r["week"], "rig": r["rig"], "thin": r["thin"] == "True", "dominant_model": r["dominant_model"] or None}
        for c in _NUMERIC:
            v = r.get(c, "")
            t[c] = None if v == "" else float(v)
        for c in ("landings", "unreadable", "manual_rescue_count", "rejections", "rho_numerator", "rho_denominator"):
            if t[c] is not None:
                t[c] = int(t[c])
        out.append(t)
    return out


def previous_weekly(out_dir):
    """The newest earlier run's weekly.csv beside out_dir (same parent, smaller name), typed; [] when there is none."""
    parent, mine = os.path.split(os.path.normpath(out_dir))
    earlier = sorted(d for d in os.listdir(parent) if d < mine and os.path.exists(os.path.join(parent, d, "weekly.csv")))
    if not earlier:
        return []
    return typed_weekly(common.read_csv(os.path.join(parent, earlier[-1], "weekly.csv")))


def report(env, result, mango_section=None):
    if mango_section is None:
        section_path = os.path.join(env.out_dir, "mango-section.md")
        mango_section = "Not run."
        if os.path.exists(section_path):
            with open(section_path) as f:
                mango_section = f.read().strip()
    backtest_text = "Not run."
    bt_path = os.path.join(env.out_dir, "backtest-section.md")
    if os.path.exists(bt_path):
        with open(bt_path) as f:
            backtest_text = f.read().strip()
    flags = render.moved_since_last(previous_weekly(env.out_dir), result["weekly"], _FLAGGED)
    events_path = os.path.join(env.docs_dir, "regime-events.json")
    events = []
    if os.path.exists(events_path):
        with open(events_path) as f:
            events = json.load(f)
    text = render.render_report(env.today, result["weekly"], flags, result["footer"], mango_section, events,
                                backtest_text)
    os.makedirs(env.docs_dir, exist_ok=True)
    path = os.path.join(env.docs_dir, env.today + "-town-metrics.md")
    with open(path, "w") as f:
        f.write(text)
    return path


# ---- real-source wiring (impure; exercised by the integration smoke, not the unit tier) ----

RIGS = ["gastown", "devops", "fractals", "beaver", "agentlab", "om", "hm", "mango", "beads"]
PREFIX_TO_RIG = {"gt": "gastown", "dv": "devops", "fr": "fractals", "bv": "beaver", "al": "agentlab", "om": "om",
                 "hm": "hm", "ma": "mango", "be": "beads", "hq": "hq", "claude": "claude"}


def _run(argv, cwd=None, timeout=120):
    """stdout of argv at low priority (the host is loaded), or None on any failure or timeout."""
    import subprocess
    try:
        r = subprocess.run(["nice", "-n", "15", *argv], cwd=cwd, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.TimeoutExpired):
        return None
    return r.stdout if r.returncode == 0 else None


def _load_json(path, default):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def _town(town=None):
    """The town root: the argument, else TOWN_METRICS_TOWN, else ~/gt."""
    return town or os.environ.get("TOWN_METRICS_TOWN") or os.path.expanduser("~/gt")


def _docs_dir():
    """Where the digest and regime-events.json live: TOWN_METRICS_DOCS, else ~/.claude/docs/metrics."""
    return os.environ.get("TOWN_METRICS_DOCS") or os.path.expanduser("~/.claude/docs/metrics")


def real_env(today, town=None, docs_dir=None):
    from . import diffstats
    town = _town(town)
    rt = os.path.join(town, ".runtime")
    cache_dir = os.path.join(rt, "metrics", "cache")
    os.makedirs(cache_dir, exist_ok=True)
    unreadable, ledger = [], {}
    for rig in RIGS:
        try:
            with open(os.path.join(rt, "landings", rig + ".jsonl")) as f:
                ledger[rig] = f.read().splitlines()
        except OSError:
            unreadable.append("ledger:" + rig)
    try:
        with open(os.path.join(rt, "landings", "reviews.jsonl")) as f:
            reviews = f.read().splitlines()
    except OSError:
        reviews = []
        unreadable.append("reviews")
    usage = {}
    udir = os.path.join(rt, "usage", "beads")
    for name in sorted(os.listdir(udir)) if os.path.isdir(udir) else []:
        if name.endswith(".work.json"):
            data = _load_json(os.path.join(udir, name), None)
            if data:
                usage[name[:-len(".work.json")]] = data
    odir = os.path.join(rt, "overrun-guard")
    overrun = {}
    for name in sorted(os.listdir(odir)) if os.path.isdir(odir) else []:
        if name.endswith(".json"):
            state = _load_json(os.path.join(odir, name), None)
            if state:
                overrun[name[:-5]] = state

    def git(rig, *args):
        return _run(["git", "--git-dir", os.path.join(town, rig, ".repo.git"), *args], timeout=60)

    om_commits = {}
    for rig in RIGS:
        log = git(rig, "log", "--reverse", "--format=%cI %H", "origin/main", "--", ".om.json")  # local main is stale in bare repos or ""
        om_commits[rig] = [(w, git(rig, "show", sha + ":.om.json") or "")
                           for w, sha in (ln.split() for ln in log.splitlines())]
    diff_cache_path = os.path.join(cache_dir, "diffstats.json")
    diff_cache = {tuple(k.split(":", 1)): v for k, v in _load_json(diff_cache_path, {}).items()}
    source = diffstats.GitDiffSource(lambda rig: os.path.join(town, rig, ".repo.git"),
                                     lambda args: _run(["git", *args], timeout=60), diff_cache)
    return Env(
        ledger_lines=ledger, review_lines=reviews, diff_source=source,
        run_bd=lambda cwd, args: _run(["bd", *args], cwd=cwd), prefix_to_rig=PREFIX_TO_RIG,
        bead_dirs=[os.path.join(town, r) for r in RIGS] + [town, os.path.expanduser("~/.claude")],
        usage_files=usage, overrun_states=overrun, om_commits=om_commits,
        out_dir=os.path.join(rt, "metrics", today),
        docs_dir=docs_dir or _docs_dir(),
        today=today, unreadable=unreadable,
        handoff_cache=_load_json(os.path.join(cache_dir, "handoff_bytes.json"), {}))


def save_caches(env, town=None):
    cache_dir = os.path.join(_town(town), ".runtime", "metrics", "cache")
    for name, data in (("handoff_bytes.json", env.handoff_cache),
                       ("diffstats.json", {":".join(k): v for k, v in env.diff_source.cache.items()})):
        with open(os.path.join(cache_dir, name), "w") as f:
            json.dump(data, f)


def write_backtest(env):
    """Backtest the weekly metrics against regime-events.json; Sloan's answers (if any) live beside it."""
    docs = env.docs_dir
    events = _load_json(os.path.join(docs, "regime-events.json"), [])
    sloan = _load_json(os.path.join(docs, "sloan-answers.json"), {})
    summary = _load_json(os.path.join(env.out_dir, "mango-summary.json"), {})
    weekly = load_result(env.out_dir)["weekly"]
    text = backtest.backtest_section(weekly, events, backtest.replicated_from_summary(summary), sloan)
    with open(os.path.join(env.out_dir, "backtest-section.md"), "w") as f:
        f.write(text + "\n")
    return text


def find_mango(town=None):
    """MANGO_BIN, else the binary built into ~/gt/.runtime/metrics/bin, else PATH; None when there is none.

    MANGO_BIN is exec'd as given (the user's own environment); run_mango only passes it fixed subcommands."""
    built = os.path.join(_town(town), ".runtime", "metrics", "bin", "mango")
    return os.environ.get("MANGO_BIN") or (built if os.path.exists(built) else None) or mango_run.find_binary()


def _run_rc(argv):
    import subprocess
    try:
        r = subprocess.run(["nice", "-n", "15", *argv], capture_output=True, text=True, timeout=900)
    except (OSError, subprocess.TimeoutExpired) as e:
        return 1, "%s: %s" % (type(e).__name__, e)
    return r.returncode, r.stdout if r.returncode == 0 else (r.stdout + r.stderr)


def write_mango_section(env):
    text = mango_run.mango_section(env.out_dir, find_mango(), _run_rc)
    with open(os.path.join(env.out_dir, "mango-section.md"), "w") as f:
        f.write(text + "\n")
    return text


def run_snapshot(town=None):
    """Append one snapshot line to ~/gt/.runtime/metrics/snapshots.jsonl and return the log's path."""
    import datetime as dt
    town = _town(town)
    path = os.path.join(town, ".runtime", "metrics", "snapshots.jsonl")
    snapshot.append(path, snapshot.take(town, _run, dt.datetime.now(dt.timezone.utc)))
    return path


def main(argv):
    import datetime as dt
    cmds = {"snapshot", "extract", "mango", "backtest", "report", "all"}
    if len(argv) != 1 or argv[0] not in cmds:
        print("usage: town-metrics snapshot|extract|mango|backtest|report|all   (run under `nice`, on demand only)")
        return 2
    cmd = argv[0]
    if cmd == "snapshot":
        print("snapshot: appended to %s" % run_snapshot())
        return 0
    env = real_env(dt.date.today().isoformat())
    if cmd in ("extract", "all"):
        result = extract(env)
        save_caches(env)
        print("extract: %d landings, %d weekly rows -> %s" % (len(result["landings"]), len(result["weekly"]), env.out_dir))
    if cmd in ("mango", "all"):
        text = write_mango_section(env)
        print("mango:", text.splitlines()[0] if text.startswith("mango binary") else "section written")
    if cmd in ("backtest", "all"):
        write_backtest(env)
        print("backtest: section written")
    if cmd in ("report", "all"):
        print("report:", report(env, load_result(env.out_dir)))
    return 0
