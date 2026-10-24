"""Loaders: each takes its source by injection (lines, dicts, callables) so no unit test touches ~/gt or bd."""
import json
import re

from .common import parse_ts

_REJECTION = re.compile(r"MERGE REJECTION \(attempt \d+\): (\w+)")
_LEDGER_KEYS = ["bead", "rig", "landed_commit", "landed_at", "route", "om_verdict", "gate_result"]


def _json_rows(lines, source, dropped):
    """Yield one parsed object per non-blank line; None (and a drop) for a line that is not JSON."""
    for line in lines:
        if not line.strip():
            continue
        try:
            yield json.loads(line)
        except ValueError:
            dropped.add(source, "unparseable json")
            yield None


def load_ledger(sources, dropped):
    rows = []
    for rig, lines in sources.items():
        for obj in _json_rows(lines, "ledger:" + rig, dropped):
            if obj is None:
                continue
            if not obj.get("bead") or not obj.get("landed_at"):
                dropped.add("ledger:" + rig, "missing bead or landed_at")
                continue
            rows.append({k: obj.get(k) for k in _LEDGER_KEYS} | {"rig": obj.get("rig") or rig})
    return rows


def load_reviews(lines, dropped):
    rows = []
    for obj in _json_rows(lines, "reviews", dropped):
        if obj is None:
            continue
        try:
            rows.append({"bead": obj["bead"], "rig": obj.get("rig"), "attempt": int(obj["attempt"]),
                         "score": float(obj["score"]), "verdict": obj["verdict"], "at": obj["at"]})
        except (KeyError, TypeError, ValueError):
            dropped.add("reviews", "missing field")
    return rows


def rework_by_bead(reviews, landed_at, note_kinds=None):
    """attempts and rejected_before_landing per landed bead.

    reviews.jsonl only starts mid-October; the MERGE REJECTION notes on the bead reach back to the first landing, so
    both count: a bead is rejected if either source says so, and attempts is the larger of the two views."""
    note_kinds = note_kinds or {}
    out = {b: {"attempts": 1 + len(note_kinds.get(b, [])), "rejected_before_landing": bool(note_kinds.get(b))}
           for b in landed_at}
    for r in reviews:
        b = r["bead"]
        if b not in landed_at:
            continue
        out[b]["attempts"] = max(out[b]["attempts"], r["attempt"])
        if r["verdict"] == "request_changes" and parse_ts(r["at"]) <= parse_ts(landed_at[b]):
            out[b]["rejected_before_landing"] = True
    return out


def _bd_json(run_bd, cwd, args):
    for _ in range(2):  # one retry, then give up
        out = run_bd(cwd, args)
        if out is not None:
            try:
                return json.loads(out)
            except ValueError:
                return None
    return None


def load_beads(run_bd, dirs, dropped):
    """One `bd list` per directory (host is loaded: never one call per bead)."""
    rows, seen = [], set()
    for d in dirs:
        data = _bd_json(run_bd, d, ["list", "--all", "--limit", "0", "--json"])
        if data is None:
            dropped.fail("bd:" + d, "bd list failed in " + d)
            continue
        for b in data:
            if b["id"] in seen:  # the same bead can be listed from two directories; the first one wins
                continue
            seen.add(b["id"])
            rows.append({
                "id": b["id"], "rig": b["id"].split("-", 1)[0], "created_at": b.get("created_at"),
                "closed_at": b.get("closed_at"), "type": b.get("issue_type"), "priority": b.get("priority"),
                "labels": b.get("labels") or [], "dependency_count": int(b.get("dependency_count") or 0),
                "comment_count": int(b.get("comment_count") or 0), "dir": d,
                "shaped": bool(b.get("acceptance_criteria")),  # proxy: a shaped bead carries acceptance criteria
                "rejection_kinds": _REJECTION.findall((b.get("notes") or "") + (b.get("description") or "")),
            })
    return rows


def handoff_bytes(run_bd, dirs_by_bead, dropped, cap=150, cache=None):
    """Bytes of notes plus comments per bead. Sequential, capped, cache-first; over-cap beads are named, not guessed."""
    out, calls = {}, 0
    cache = {} if cache is None else cache   # the caller's dict, so fresh sizes persist
    for bead, cwd in dirs_by_bead.items():
        if bead in cache:
            out[bead] = cache[bead]
            continue
        if calls >= cap:
            dropped.add("bd:" + bead, "not fetched: call cap %d reached" % cap)
            continue
        calls += 1
        data = _bd_json(run_bd, cwd, ["show", bead, "--json"])
        if not data:
            dropped.add("bd:" + bead, "bd show failed")
            continue
        b = data[0]
        out[bead] = len((b.get("notes") or "").encode()) + sum(
            len((c.get("text") or "").encode()) for c in b.get("comments") or [])
        cache[bead] = out[bead]
    return out


def load_usage(files):
    out = {}
    for bead, f in files.items():
        models = f.get("models") or {}
        if not models:
            continue
        top = max(sorted(models), key=lambda m: models[m].get("calls", 0))
        out[bead] = {"model": top, "seat": f.get("seat"), "end": f.get("end"),
                     "sessions": len(f.get("sessions") or [])}
    return out


def load_overrun(states, bead_ids):
    """Beads whose overrun stop began. The guard's state file exists for every bead it ever tracked (nudges, gate
    accounting), so existence means nothing: `stop_began_at` is the stamp written when the pause marker goes down."""
    zero = "0001-01-01T00:00:00Z"
    return {b for b in bead_ids if (states.get(b) or {}).get("stop_began_at", zero) not in (zero, "", None)}


def om_eras(commits):
    eras = []
    for when, text in commits:
        try:
            t = json.loads(text).get("threshold")
        except (ValueError, AttributeError):
            continue
        if t is not None:
            eras.append((when, float(t)))
    return eras


def era_of(eras, ts):  # eras: [(iso time, threshold)] oldest first
    cur = None
    for start, t in eras:
        if parse_ts(start) <= parse_ts(ts):
            cur = t
    return cur
