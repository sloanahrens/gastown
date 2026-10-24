"""Join loader outputs into landings.csv rows. Pure: every source arrives as an argument."""
import json

from .common import week_of
from .loaders import era_of

LANDING_COLUMNS = ["bead", "rig", "landed_at", "week", "route", "om_verdict", "gate_result", "lines_added",
                   "lines_deleted", "lines_total", "files_touched", "file_entropy", "package_entropy", "attempts",
                   "rejected_before_landing", "manual_rescue", "reached_overrun_stop", "unreadable", "type",
                   "priority", "shaped", "label_count", "model", "om_threshold", "dominant_model"]
# Known only after the landing: never a predictor, only joined back to explain a box.
EXPLAIN_ONLY = {"lines_added", "lines_deleted", "lines_total", "files_touched", "file_entropy", "package_entropy",
                "attempts", "gate_result", "om_verdict", "unreadable"}


def build_landings(ledger, rework, diff_source, beads_by_id, usage, overrun, eras, shaped_ids, dropped):
    rows = []
    for l in ledger:
        b = beads_by_id.get(l["bead"], {})
        rw = rework.get(l["bead"], {"attempts": 1, "rejected_before_landing": False})
        d = diff_source.stats(l["rig"], l["landed_commit"]) if l.get("landed_commit") else None
        if d is None:
            dropped.add("git:" + l["rig"], "unreadable diff for " + l["bead"])
        model = (usage.get(l["bead"]) or {}).get("model")
        rows.append({
            "bead": l["bead"], "rig": l["rig"], "landed_at": l["landed_at"], "week": week_of(l["landed_at"]),
            "route": l["route"], "om_verdict": l["om_verdict"], "gate_result": l["gate_result"],
            "lines_added": d and d["lines_added"], "lines_deleted": d and d["lines_deleted"],
            "lines_total": d and d["lines_added"] + d["lines_deleted"],
            "files_touched": d and d["files_touched"], "file_entropy": d and d["file_entropy"],
            "package_entropy": d and d["package_entropy"],
            "attempts": rw["attempts"], "rejected_before_landing": int(rw["rejected_before_landing"]),
            "manual_rescue": int(l["route"] != "daemon"), "reached_overrun_stop": int(l["bead"] in overrun),
            "unreadable": int(d is None), "type": b.get("type"), "priority": b.get("priority"),
            "shaped": int(l["bead"] in shaped_ids), "label_count": len(b.get("labels") or []),
            "model": model, "om_threshold": era_of(eras.get(l["rig"], []), l["landed_at"]), "dominant_model": model,
        })
    return rows


def write_landing_sidecar(path):
    with open(path, "w") as f:
        json.dump({"explain_only": sorted(EXPLAIN_ONLY)}, f, indent=1)
