#!/usr/bin/env python3
"""resource-sampler: CPU and memory for the town's workloads, schema v1.

One JSON object per line every 30 s, appended to
`<metrics>/resources/<UTC date>.jsonl`, plus one per-sandbox peak row in
`<metrics>/resources/bead-peaks.jsonl` when a sandbox container goes away.
mango's town_metrics reads the same files, so the record shape is fixed by
the v1 schema agreed with snipe (gt-8zpjn):

    v, ts, interval_s, source, class, component, name, rig, polecat, bead,
    cpu_pct, mem_bytes, mem_limit_bytes

`cpu_pct` is percent of ONE core (100 = one full core), memory is bytes, and
an unknown value is null, never "" or 0.

Sources: the host's `ps`, docker containers (each context named explicitly),
and `kubectl top pod -A` when a kubeconfig is given. One failing source is
reported on stderr and the others still run. Host rows are one per component,
summed over that component's processes; `name` is the process that used the
most CPU that tick (the most memory on the first tick, when no delta exists).
Container and pod rows are one per container or pod. A container or pod named
for a session (`<rig prefix>-<polecat>`, prefix resolved through the town's
`routes.jsonl`), or a container carrying the create's `rig` and `polecat`
labels, is classed sandbox and its bead read from beads.

Sandbox peaks come from the container's own cgroup, not from the 30 s
snapshots: a 30 s sample showed 204 MiB for a sandbox whose cgroup peak was
5.1 GiB. `docker stats` is the fallback when the cgroup cannot be read.

Usage:
    scripts/resource-sampler.py [--once] [--now ISO8601] [--interval S]
                                 [--metrics-dir PATH] [--town-root PATH]
"""

import argparse
import datetime as dt
import gzip
import json
import os
import re
import shutil
import subprocess
import sys
import time

SCHEMA_VERSION = 1
DEFAULT_INTERVAL_S = 30
DEFAULT_METRICS_DIR = "~/gt/.runtime/metrics"
DEFAULT_TOWN_ROOT = "~/gt"
PEAK_FILENAME = "bead-peaks.jsonl"
ROWS_SUBDIR = "resources"
STATE_FILENAME = ".state.json"
DEFAULT_DOCKER_CONTEXT = "desktop-linux"

# Bead lookups: a lookup bd answered (even with no rows) spends one of the
# first budget; a bd call that failed spends one of the second. Keeping them
# apart means a Dolt outage does not look like "this sandbox has no bead" and
# burn the retries the hooked bead needed (gt-8zpjn).
BEAD_EMPTY_ATTEMPTS = 5
BEAD_ERROR_ATTEMPTS = 10

# A cgroup "unlimited" limit is a page-rounded huge number, not a memory size.
CGROUP_UNLIMITED = 1 << 60
GZIP_AFTER_S = 2 * 86400
DELETE_GZ_AFTER_S = 14 * 86400

HOST_COMPONENT_CLASS = {
    "dolt": "control-plane",
    "gtbd": "control-plane",
    "tests": "ci",
    "claude": "operator",
    "vm": "operator",
    "av": "operator",
    "spotlight": "operator",
    "kernel": "operator",
    "other": "other",
}

# Longest first so "forgejo-db" wins over "forgejo" in a container name.
ROLE_COMPONENTS = (
    "forgejo-db",
    "ci-engine",
    "ci-runner",
    "hm-postgres",
    "supervisor",
    "forgejo",
    "gateway",
    "sandbox",
)

ROLE_COMPONENT_CLASS = {
    "sandbox": "sandbox",
    "supervisor": "control-plane",
    "forgejo": "control-plane",
    "forgejo-db": "control-plane",
    "ci-engine": "ci",
    "ci-runner": "ci",
    "hm-postgres": "control-plane",
    "gateway": "control-plane",
}

# A docker container named for a session: <rig prefix>-<crewname or polecat>.
SESSION_RE = re.compile(
    r"^(?P<prefix>[a-z][a-z0-9]*)-(?:(?P<kind>crew)-)?"
    r"(?P<name>[A-Za-z0-9][A-Za-z0-9_-]*)$"
)

# Field order of a 30 s row and of a peak row. json.dumps on a dict keeps the
# insertion order, so building every row from these gives the reader one shape.
ROW_FIELDS = (
    "v",
    "ts",
    "interval_s",
    "source",
    "class",
    "component",
    "name",
    "rig",
    "polecat",
    "bead",
    "cpu_pct",
    "mem_bytes",
    "mem_limit_bytes",
)
PEAK_FIELDS = (
    "v",
    "bead",
    "rig",
    "polecat",
    "sandbox",
    "profile",
    "started_at",
    "ended_at",
    "end_state",
    "peak_mem_bytes",
    "peak_cpu_pct",
    "mem_limit_bytes",
    "samples",
    "source",
)

PS_ARGS = ["ps", "-axo", "pid=,rss=,cputime=,etime=,command="]
# `rig` and `polecat` are the labels a sandbox create carries
# (internal/session/backend_openshell.go); the profile label is
# opportunistic, and a bead always comes from beads, never from a label.
DOCKER_PS_FORMAT = "\t".join(
    (
        "{{.ID}}",
        "{{.Names}}",
        '{{.Label "rig"}}',
        '{{.Label "polecat"}}',
        '{{.Label "profile"}}',
    )
)
DOCKER_STATS_FORMAT = "\t".join(
    ("{{.ID}}", "{{.Name}}", "{{.CPUPerc}}", "{{.MemUsage}}")
)
# Label the memory values, so one missing file is an empty value the parser can
# tell apart from a zero; cpu.stat is passed through whole because it holds
# several counters and only the caller knows which one it needs. `sh` is the
# one program a distroless image may lack; its failure is the stats fallback.
CGROUP_CMD = (
    "for f in memory.peak memory.current memory.max; do "
    'printf "%s=%s\\n" "$f" "$(cat /sys/fs/cgroup/$f 2>/dev/null)"; done; '
    "printf 'cpu.stat\\n'; cat /sys/fs/cgroup/cpu.stat 2>/dev/null; "
    "printf 'cpu.stat.end\\n'"
)


class SourceError(Exception):
    """A sampling source failed; the tick keeps the other sources."""


def iso(now):
    """The schema's timestamp: UTC, ISO-8601, trailing Z, second precision."""
    return now.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_iso(text):
    """A datetime for an ISO-8601 value, or None. Any precision and offset."""
    text = text.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    # fromisoformat took at most 6 fractional digits before Python 3.11.
    text = re.sub(r"\.(\d{6})\d+", r".\1", text)
    try:
        when = dt.datetime.fromisoformat(text)
    except ValueError:
        return None
    if when.tzinfo is None:
        when = when.replace(tzinfo=dt.timezone.utc)
    return when.astimezone(dt.timezone.utc)


def parse_clock(text):
    """Seconds in a ps time field: [[dd-]hh:]mm:ss[.frac]."""
    text = text.strip()
    if not text:
        return None
    days = 0
    if "-" in text:
        head, _, text = text.partition("-")
        try:
            days = int(head)
        except ValueError:
            return None
    parts = text.split(":")
    if len(parts) > 3 or not parts:
        return None
    try:
        nums = [float(p) for p in parts]
    except ValueError:
        return None
    nums = [0.0] * (3 - len(nums)) + nums
    return days * 86400 + nums[0] * 3600 + nums[1] * 60 + nums[2]


SIZE_RE = re.compile(r"^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)$")
SIZE_UNITS = {
    "": 1,
    "b": 1,
    "k": 1000,
    "kb": 1000,
    "kib": 1024,
    "ki": 1024,
    "kib": 1024,
    "m": 1000**2,
    "mb": 1000**2,
    "mi": 1024**2,
    "mib": 1024**2,
    "g": 1000**3,
    "gb": 1000**3,
    "gi": 1024**3,
    "gib": 1024**3,
    "t": 1000**4,
    "tb": 1000**4,
    "ti": 1024**4,
    "tib": 1024**4,
    "p": 1000**5,
    "pi": 1024**5,
}


def parse_size(text):
    """Bytes in a docker/kubectl size ("204.5MiB", "7.6GiB", "123456"), or None."""
    if text is None:
        return None
    text = text.strip()
    if not text or text.lower() in ("n/a", "<no value>"):
        return None
    m = SIZE_RE.match(text)
    if not m:
        return None
    unit = m.group(2).lower()
    if unit not in SIZE_UNITS:
        return None
    return int(float(m.group(1)) * SIZE_UNITS[unit])


def parse_k8s_cpu(text):
    """Percent of one core from a kubectl top CPU quantity ("250m", "2")."""
    text = (text or "").strip()
    if not text:
        return None
    suffixes = (("n", 1e-9), ("u", 1e-6), ("m", 1e-3), ("", 1.0))
    for suffix, scale in suffixes:
        if suffix and not text.endswith(suffix):
            continue
        number = text[: len(text) - len(suffix)] if suffix else text
        try:
            return float(number) * scale * 100.0
        except ValueError:
            return None
    return None


def parse_percent(text):
    """A docker stats CPU percentage ("12.34%") as a float, or None."""
    text = (text or "").strip()
    if not text.endswith("%"):
        return None
    try:
        return float(text[:-1])
    except ValueError:
        return None


def host_component(argv0, command):
    """The loadprof component for a host process."""
    base = os.path.basename(argv0).lower()
    low = command.lower()
    if "dolt" in base:
        return "dolt"
    # The CLI sometimes runs under node, so the command's own path decides.
    if base == "claude" or base.startswith("claude-") or (
        base in ("node", "nodejs") and "claude" in low
    ):
        return "claude"
    if base in ("gt", "bd"):
        return "gtbd"
    if base in ("go", "make", "compile", "link", "asm", "cgo", "vet") or (
        "/go-build" in low or "/go/pkg/tool/" in low
    ):
        return "tests"
    if base.startswith("kernel_task"):
        return "kernel"
    if any(tok in base for tok in ("mds", "mdworker", "spotlight")):
        return "spotlight"
    if any(
        tok in base
        for tok in (
            "xprotect",
            "falcon",
            "sentinel",
            "crowdstrike",
            "sophos",
            "clamd",
            "eset",
            "mcafee",
        )
    ):
        return "av"
    if any(
        tok in base
        for tok in ("qemu", "vmm", "vmnet", "hyperkit", "virtualiz", "lima")
    ):
        return "vm"
    return "other"


def role_component(name):
    """The role component a container or pod name names, or None."""
    low = name.lower()
    for role in ROLE_COMPONENTS:
        if (
            low == role
            or low.startswith(role + "-")
            or low.endswith("-" + role)
            or ("-" + role + "-") in low
        ):
            return role
    return None


def row(**fields):
    out = {}
    for field in ROW_FIELDS:
        out[field] = fields.get(field)
    out["v"] = SCHEMA_VERSION
    return out


def peak_row(**fields):
    out = {}
    for field in PEAK_FIELDS:
        out[field] = fields.get(field)
    out["v"] = SCHEMA_VERSION
    return out


def note(message):
    print("resource-sampler: " + message, file=sys.stderr, flush=True)


def fresh_state():
    return {
        "v": SCHEMA_VERSION,
        "host": {"pids": {}, "last_ts": None},
        "containers": {},
        "sandboxes": {},
        # Peak rows wait here until the write succeeds, so a full disk or a
        # crash between finalizing a sandbox and appending its row loses no
        # bead measurement.
        "pending_peaks": [],
        "housekeeping_date": None,
    }


def _is_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def state_ok(state):
    """Whether a loaded state has the shape this version writes.

    A shape check, not a migration: a state from an older or broken run is
    moved aside rather than trusted, so one bad file cannot crash every tick
    (gt-8zpjn).
    """
    if not isinstance(state, dict) or state.get("v") != SCHEMA_VERSION:
        return False
    host = state.get("host")
    if not isinstance(host, dict):
        return False
    pids = host.get("pids")
    if not isinstance(pids, dict):
        return False
    for entry in pids.values():
        if not isinstance(entry, dict) or not isinstance(entry.get("cmd"), str):
            return False
        if not _is_number(entry.get("cpu")) or not _is_number(entry.get("etime")):
            return False
    if host.get("last_ts") is not None and parse_iso(str(host["last_ts"])) is None:
        return False
    for name in ("containers", "sandboxes"):
        group = state.get(name)
        if not isinstance(group, dict):
            return False
        for entry in group.values():
            if not isinstance(entry, dict):
                return False
    for entry in state["sandboxes"].values():
        if not isinstance(entry.get("samples"), int):
            return False
    pending = state.get("pending_peaks", [])
    if not isinstance(pending, list) or any(
        not isinstance(item, dict) for item in pending
    ):
        return False
    if state.get("housekeeping_date") is not None and not isinstance(
        state.get("housekeeping_date"), str
    ):
        return False
    return True


def load_state(path):
    """The persisted state, a fresh one, and a note when the old one was bad."""
    try:
        with open(path, "r", encoding="utf-8") as handle:
            state = json.load(handle)
    except FileNotFoundError:
        return fresh_state(), None
    except (OSError, ValueError) as exc:
        state, message = None, "state unreadable (%s); starting fresh" % exc
    else:
        message = None
        if not state_ok(state):
            message = "state shape is not v%d; starting fresh" % SCHEMA_VERSION
            state = None
    if state is not None:
        # A state written before the pending-peak queue existed is still v1
        # and keeps its running sandboxes.
        state.setdefault("pending_peaks", [])
        return state, None
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup = path + ".corrupt-" + stamp
    suffix = 1
    while os.path.exists(backup):
        suffix += 1
        backup = path + ".corrupt-" + stamp + "-%d" % suffix
    try:
        os.replace(path, backup)
        message += " (old file kept at %s)" % os.path.basename(backup)
    except OSError:
        pass
    return fresh_state(), message


class Sampler:
    def __init__(self, options):
        self.interval = options.interval
        self.fixed_now = options.now
        self.metrics_dir = os.path.expanduser(options.metrics_dir)
        self.resources_dir = os.path.join(self.metrics_dir, ROWS_SUBDIR)
        self.state_path = os.path.join(self.resources_dir, STATE_FILENAME)
        self.peak_path = os.path.join(self.resources_dir, PEAK_FILENAME)
        town = options.town_root or os.environ.get("GT_TOWN_ROOT") or DEFAULT_TOWN_ROOT
        self.town_root = os.path.expanduser(town)
        self.state, problem = load_state(self.state_path)
        if problem:
            note(problem)
        self._noted = set()
        self._prefixes = None
        self._stats_cache = {}
        self._stats_failed = set()

    # -- infrastructure -------------------------------------------------

    def now(self):
        if self.fixed_now is not None:
            return self.fixed_now
        return dt.datetime.now(dt.timezone.utc)

    def note_once(self, message):
        if message in self._noted:
            return
        self._noted.add(message)
        note(message)

    def run(self, argv, timeout):
        """The command's exit status and output; never raises for a failed run."""
        try:
            done = subprocess.run(
                argv,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.SubprocessError) as exc:
            return 127, "", str(exc)
        return (
            done.returncode,
            done.stdout.decode("utf-8", "replace"),
            done.stderr.decode("utf-8", "replace"),
        )

    def interval_seconds(self, now):
        last = self.state["host"].get("last_ts")
        if last:
            when = parse_iso(last)
            if when is not None:
                seconds = (now - when).total_seconds()
                if seconds > 0:
                    return round(seconds, 1)
        return float(self.interval)

    def save_state(self):
        os.makedirs(self.resources_dir, exist_ok=True)
        tmp = self.state_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as handle:
            json.dump(self.state, handle, separators=(",", ":"), sort_keys=True)
            handle.write("\n")
        os.replace(tmp, self.state_path)

    def append_rows(self, path, rows):
        if not rows:
            return
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "a", encoding="utf-8") as handle:
            for item in rows:
                handle.write(
                    json.dumps(item, separators=(",", ":"), ensure_ascii=True) + "\n"
                )

    def flush_peaks(self):
        """Append the peaks a finished sandbox left, then forget them."""
        pending = self.state.get("pending_peaks") or []
        if not pending:
            return
        lines = [
            json.dumps(item, separators=(",", ":"), ensure_ascii=True)
            for item in pending
        ]
        # A crash between a successful append and the state save leaves the
        # same rows pending; the file's tail says whether they landed.
        if not self.tail_matches(self.peak_path, lines):
            self.append_rows(self.peak_path, pending)
        self.state["pending_peaks"] = []

    @staticmethod
    def tail_matches(path, lines):
        try:
            size = os.path.getsize(path)
            with open(path, "rb") as handle:
                window = 64 * 1024
                if size > window:
                    handle.seek(size - window)
                data = handle.read().decode("utf-8", "replace")
        except OSError:
            return False
        tail = [line for line in data.splitlines() if line.strip()]
        if size > 64 * 1024 and tail:
            tail = tail[1:]  # the window may start mid-line
        return tail[-len(lines):] == lines

    def prefixes(self):
        """Rig prefix -> rig name and root, read from the town's routes."""
        if self._prefixes is not None:
            return self._prefixes
        found = {}
        path = os.path.join(self.town_root, ".beads", "routes.jsonl")
        try:
            with open(path, "r", encoding="utf-8") as handle:
                for line in handle:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        route = json.loads(line)
                    except ValueError:
                        continue
                    prefix = str(route.get("prefix", "")).rstrip("-")
                    rel = str(route.get("path", ""))
                    if not prefix or not rel:
                        continue
                    if rel in (".", ""):
                        rig = os.path.basename(self.town_root) or prefix
                        root = self.town_root
                    else:
                        parts = rel.split("/")
                        rig = parts[0]
                        root = os.path.join(self.town_root, *parts)
                        if parts[-2:] == ["mayor", "rig"]:
                            root = os.path.join(self.town_root, *parts[:-2]) or "/"
                    found[prefix] = {"rig": rig, "root": root}
        except OSError as exc:
            self.note_once("routes.jsonl unreadable (%s); sandboxes stay unnamed" % exc)
        self._prefixes = found
        return found

    # -- the tick -------------------------------------------------------

    def tick(self):
        now = self.now()
        stamp = iso(now)
        interval = self.interval_seconds(now)
        self.state["host"]["last_ts"] = stamp
        # A stats read serves one tick's fallback only: caching it for the
        # process would leave a container that starts later without numbers.
        self._stats_cache = {}
        self._stats_failed = set()

        rows = []
        for name, sample in (
            ("host", self.sample_host),
            ("docker", self.sample_docker),
            ("k8s", self.sample_k8s),
        ):
            try:
                rows.extend(sample(now, interval, stamp))
            except SourceError as exc:
                self.note_once("source %s: %s" % (name, exc))
            except Exception as exc:  # one source must never end the loop
                self.note_once(
                    "source %s: %s: %s" % (name, type(exc).__name__, exc)
                )

        try:
            self.flush_peaks()
        except OSError as exc:
            note("could not append peak rows; keeping them for the next tick: %s" % exc)
        try:
            self.append_rows(
                os.path.join(self.resources_dir, now.strftime("%Y-%m-%d") + ".jsonl"),
                rows,
            )
        except OSError as exc:
            note("could not append rows: %s" % exc)
        try:
            self.housekeeping(now)
        except OSError as exc:
            self.note_once("housekeeping: %s" % exc)
        try:
            self.save_state()
        except OSError as exc:
            note("could not save state: %s" % exc)

    # -- host -----------------------------------------------------------

    def sample_host(self, now, interval, stamp):
        code, out, err = self.run(PS_ARGS, timeout=20)
        if code != 0:
            # The next success has no baseline it can trust across the gap:
            # dividing a two-tick CPU delta by one interval would invent a
            # percentage. Dropping the baselines reports null CPU once instead.
            self.state["host"]["pids"] = {}
            raise SourceError("ps exited %d: %s" % (code, err.strip()[:120]))

        previous = self.state["host"].get("pids", {})
        current = {}
        aggregates = {}
        for line in out.splitlines():
            parts = line.split(None, 4)
            if len(parts) < 5:
                continue
            pid, rss_kb, cpu_text, etime_text, command = parts
            if not pid.isdigit():
                continue
            cpu = parse_clock(cpu_text)
            etime = parse_clock(etime_text)
            if cpu is None or etime is None:
                continue
            try:
                mem = int(rss_kb) * 1024
            except ValueError:
                continue
            argv0 = command.split()[0] if command.split() else command
            ident = os.path.basename(argv0)
            current[pid] = {"cpu": cpu, "etime": etime, "cmd": ident}

            cpu_pct = None
            seen = previous.get(pid)
            if (
                interval > 0
                and seen
                and seen.get("cmd") == ident
                and _is_number(seen.get("cpu"))
                and _is_number(seen.get("etime"))
                and etime >= seen["etime"]
                and cpu >= seen["cpu"]
            ):
                # A pid whose elapsed time went backwards was reused by a new
                # process; its CPU total is not a delta of the old process.
                cpu_pct = (cpu - seen["cpu"]) / interval * 100.0

            component = host_component(argv0, command)
            agg = aggregates.setdefault(
                component,
                {"cpu": None, "mem": 0, "member": ident, "best": (-1.0, -1)},
            )
            agg["mem"] += mem
            if cpu_pct is not None:
                agg["cpu"] = (agg["cpu"] or 0.0) + cpu_pct
            marker = (cpu_pct if cpu_pct is not None else -1.0, mem)
            if marker > agg["best"]:
                agg["best"] = marker
                agg["member"] = ident

        self.state["host"]["pids"] = current

        rows = []
        for component in sorted(aggregates):
            agg = aggregates[component]
            rows.append(
                row(
                    ts=stamp,
                    interval_s=interval,
                    source="host",
                    **{
                        "class": HOST_COMPONENT_CLASS.get(component, "other"),
                        "component": component,
                        "name": agg["member"],
                        "rig": None,
                        "polecat": None,
                        "bead": None,
                        "cpu_pct": None
                        if agg["cpu"] is None
                        else round(agg["cpu"], 2),
                        "mem_bytes": agg["mem"],
                        "mem_limit_bytes": None,
                    },
                )
            )
        return rows

    # -- docker ---------------------------------------------------------

    def docker_contexts(self):
        contexts = [DEFAULT_DOCKER_CONTEXT]
        extra = os.environ.get("GT_SAMPLER_DOCKER_CONTEXTS", "")
        for context in extra.split(","):
            context = context.strip()
            if context and context not in contexts:
                contexts.append(context)
        return contexts

    def docker(self, context, args, timeout=15):
        return self.run(
            ["docker", "--context", context] + list(args), timeout=timeout
        )

    def docker_list(self, context):
        """Running containers in the context, or None when docker failed."""
        code, out, err = self.docker(
            context,
            ["ps", "--no-trunc", "--format", DOCKER_PS_FORMAT],
        )
        if code != 0:
            self.note_once(
                "docker: ps failed in context %s: %s" % (context, err.strip()[:120])
            )
            return None
        entries = []
        for line in out.splitlines():
            if not line.strip():
                continue
            fields = (line.split("\t") + [""] * 5)[:5]
            entry = {
                "id": fields[0].strip(),
                "name": fields[1].split(",")[0].strip(),
                "rig": fields[2].strip(),
                "polecat": fields[3].strip(),
                "profile": fields[4].strip(),
            }
            for key in ("rig", "polecat", "profile"):
                if entry[key] in ("<no value>", "N/A"):
                    entry[key] = ""
            if entry["id"] and entry["name"]:
                entries.append(entry)
        return entries

    def docker_stats(self, context):
        """Container name -> (cpu percent of one core, bytes, limit bytes)."""
        if context in self._stats_cache:
            return self._stats_cache[context]
        if context in self._stats_failed:
            return {}
        code, out, err = self.docker(
            context, ["stats", "--no-stream", "--format", DOCKER_STATS_FORMAT]
        )
        if code != 0:
            self._stats_failed.add(context)
            self.note_once(
                "docker: stats failed in context %s: %s"
                % (context, err.strip()[:120])
            )
            return {}
        stats = {}
        for line in out.splitlines():
            fields = (line.split("\t") + [""] * 4)[:4]
            _cid, name, cpu_text, mem_text = fields
            usage, _, limit = mem_text.partition("/")
            pct = parse_percent(cpu_text)
            # docker stats already normalizes by the host's CPU count, so its
            # 100 is one core, the same unit as schema v1 (gt-8zpjn).
            stats[name.strip().lstrip("/")] = (
                pct,
                parse_size(usage),
                parse_size(limit),
            )
        self._stats_cache[context] = stats
        return stats

    def cgroup(self, context, container_id):
        """The cgroup's usage and memory readings, or None when unreadable."""
        code, out, _ = self.docker(
            context, ["exec", container_id, "sh", "-c", CGROUP_CMD]
        )
        if code != 0:
            return None
        values = {}
        in_cpu_stat = False
        for line in out.splitlines():
            if line.strip() == "cpu.stat":
                in_cpu_stat = True
                continue
            if line.strip() == "cpu.stat.end":
                in_cpu_stat = False
                continue
            if in_cpu_stat:
                fields = line.replace("=", " ").split()
                if (
                    len(fields) >= 2
                    and fields[0] == "usage_usec"
                    and fields[1].isdigit()
                ):
                    values["usage_usec"] = int(fields[1])
                continue
            key, _, value = line.partition("=")
            value = value.strip()
            values[key.strip()] = int(value) if value.isdigit() else None
        usage = values.get("usage_usec")
        current = values.get("memory.current")
        if usage is None and current is None:
            return None
        limit = values.get("memory.max")
        if limit is not None and limit >= CGROUP_UNLIMITED:
            limit = None
        return {
            "usage_usec": usage,
            "mem_bytes": current,
            "mem_peak": values.get("memory.peak"),
            "mem_limit": limit,
        }

    def sandbox_identity(self, entry):
        """(rig, polecat, kind) for a session-named container, or None."""
        match = SESSION_RE.match(entry["name"])
        kind = "crew" if match and match.group("kind") == "crew" else "polecats"
        if entry["rig"] and entry["polecat"]:
            return entry["rig"], entry["polecat"], kind
        if not match:
            return None
        route = self.prefixes().get(match.group("prefix"))
        if route is None:
            return None
        return route["rig"], match.group("name"), kind

    def rig_route(self, rig):
        for route in self.prefixes().values():
            if route["rig"] == rig:
                return route
        return None

    def lookup_bead(self, sandbox):
        """("found", id) | ("empty", None) | ("error", message)."""
        route = self.rig_route(sandbox["rig"])
        root = route["root"] if route else None
        if root is None:
            return "error", "no route for rig %s" % sandbox["rig"]
        assignee = "%s/%s/%s" % (
            sandbox["rig"],
            sandbox.get("kind") or "polecats",
            sandbox["polecat"],
        )
        code, out, err = self.run(
            [
                "bd",
                "-C",
                root,
                "list",
                "--assignee",
                assignee,
                "--status",
                "open,in_progress,hooked",
                "--json",
                "--limit",
                "5",
            ],
            timeout=10,
        )
        if code != 0:
            return "error", "bd exited %d: %s" % (code, err.strip()[:120])
        try:
            issues = json.loads(out or "[]")
        except ValueError:
            return "error", "bd output is not JSON"
        if not isinstance(issues, list):
            return "error", "bd output is not a list"
        order = {"hooked": 0, "in_progress": 1, "open": 2}
        usable = [i for i in issues if isinstance(i, dict) and i.get("id")]
        if not usable:
            return "empty", None
        usable.sort(key=lambda i: order.get(i.get("status"), 9))
        return "found", str(usable[0]["id"])

    def start_of(self, context, container_id):
        """The container's start time in schema form, or None."""
        code, out, _ = self.docker(
            context, ["inspect", "--format", "{{.State.StartedAt}}", container_id]
        )
        if code != 0:
            return None
        when = parse_iso(out.strip())
        return iso(when) if when is not None else None

    def sample_docker(self, now, interval, stamp):
        rows = []
        listed = []
        seen = set()
        containers = self.state.setdefault("containers", {})

        for context in self.docker_contexts():
            entries = self.docker_list(context)
            if entries is None:
                # A failed listing says nothing about which sandboxes ended;
                # finalizing them here would invent an end for every one. It
                # does drop this context's CPU baselines: a delta spanning the
                # gap must not be divided by one interval.
                for key in list(containers):
                    if key.split("\t", 1)[0] == context:
                        del containers[key]
                continue
            listed.append(context)
            for entry in entries:
                key = context + "\t" + entry["id"]
                seen.add(key)
                try:
                    rows.append(
                        self.docker_row(
                            context, entry, key, interval, stamp, containers
                        )
                    )
                except Exception as exc:  # one container cannot end the source
                    self.note_once(
                        "docker: %s: %s: %s"
                        % (entry["name"], type(exc).__name__, exc)
                    )

        if not listed:
            raise SourceError("no docker context could list containers")

        # Only contexts that listed successfully can prove an end; another
        # context's sandbox may still be running.
        for key in list(self.state.get("sandboxes", {})):
            record = self.state["sandboxes"][key]
            if record.get("context") not in listed or key in seen:
                continue
            self.state["pending_peaks"].append(self.finalize_sandbox(record, stamp))
            del self.state["sandboxes"][key]

        for key in list(containers):
            context = key.split("\t", 1)[0]
            if context in listed and key not in seen:
                del containers[key]

        return rows

    def docker_row(self, context, entry, key, interval, stamp, containers):
        component = role_component(entry["name"])
        identity = None
        if component is None:
            identity = self.sandbox_identity(entry)
            component = "sandbox" if identity is not None else "other"
        cls = ROLE_COMPONENT_CLASS.get(component, "other")
        rig = polecat = bead = None
        if identity is not None:
            rig, polecat, kind = identity
        else:
            kind = None
            rig = entry["rig"] or None
            polecat = entry["polecat"] or None

        reading = self.cgroup(context, entry["id"])
        previous = containers.get(key) or {}
        restarted = False
        if reading is None:
            # A cgroup that cannot be read leaves no baseline for the next
            # tick; keeping the old one would charge a restart as CPU.
            containers.pop(key, None)
            fallback = self.docker_stats(context).get(entry["name"])
            if fallback is not None:
                cpu_pct, mem_bytes, mem_limit = fallback
                mem_peak = None
            else:
                cpu_pct = mem_bytes = mem_limit = mem_peak = None
        else:
            usage = reading["usage_usec"]
            prior = previous.get("usage_usec")
            # A cgroup counter that went backwards is a container that
            # restarted under the same id: the old sandbox ended there.
            restarted = (
                usage is not None and _is_number(prior) and usage < prior
            )
            cpu_pct = None
            if (
                interval > 0
                and usage is not None
                and _is_number(prior)
                and not restarted
            ):
                cpu_pct = (usage - prior) / 1_000_000.0 / interval * 100.0
            mem_bytes = reading["mem_bytes"]
            mem_limit = reading["mem_limit"]
            mem_peak = reading["mem_peak"]
            containers[key] = {"usage_usec": usage}

        if not identity:
            return row(
                ts=stamp,
                interval_s=interval,
                source="docker",
                **{
                    "class": cls,
                    "component": component,
                    "name": entry["name"],
                    "rig": rig,
                    "polecat": polecat,
                    "bead": bead,
                    "cpu_pct": None if cpu_pct is None else round(cpu_pct, 2),
                    "mem_bytes": mem_bytes,
                    "mem_limit_bytes": mem_limit,
                },
            )

        if restarted:
            record = self.state.get("sandboxes", {}).pop(key, None)
            if record is not None:
                self.state["pending_peaks"].append(
                    self.finalize_sandbox(record, stamp)
                )

        record = self.sandbox_record(key, context, entry, identity, kind, stamp)
        self.update_peak(record, cpu_pct, mem_bytes, mem_peak, mem_limit)
        self.refresh_bead(record)
        return row(
            ts=stamp,
            interval_s=interval,
            source="docker",
            **{
                "class": cls,
                "component": component,
                "name": entry["name"],
                "rig": record.get("rig"),
                "polecat": record.get("polecat"),
                "bead": record.get("bead"),
                "cpu_pct": None if cpu_pct is None else round(cpu_pct, 2),
                "mem_bytes": mem_bytes,
                "mem_limit_bytes": mem_limit,
            },
        )

    def sandbox_record(self, key, context, entry, identity, kind, stamp):
        sandboxes = self.state.setdefault("sandboxes", {})
        record = sandboxes.get(key)
        rig, polecat, _ = identity
        if record is None:
            record = {
                "context": context,
                "id": entry["id"],
                "name": entry["name"],
                "rig": rig,
                "polecat": polecat,
                "kind": kind,
                "bead": None,
                "profile": entry["profile"] or None,
                "started_at": self.start_of(context, entry["id"]) or stamp,
                "last_seen": stamp,
                "peak_mem_bytes": None,
                "peak_cpu_pct": None,
                "mem_limit_bytes": None,
                "samples": 0,
                "bead_empty_attempts": 0,
                "bead_error_attempts": 0,
                "bead_given_up": False,
            }
            sandboxes[key] = record
        else:
            record["last_seen"] = stamp
            if not record.get("rig"):
                record["rig"] = rig
            if not record.get("polecat"):
                record["polecat"] = polecat
            if not record.get("profile"):
                record["profile"] = entry["profile"] or None
        record["samples"] = int(record.get("samples") or 0) + 1
        return record

    def update_peak(self, record, cpu_pct, mem_bytes, mem_peak, mem_limit):
        observed = [value for value in (mem_peak, mem_bytes) if value is not None]
        if observed:
            high = max(observed)
            prior = record.get("peak_mem_bytes")
            record["peak_mem_bytes"] = high if prior is None else max(prior, high)
        if cpu_pct is not None:
            prior = record.get("peak_cpu_pct")
            record["peak_cpu_pct"] = (
                round(cpu_pct, 2) if prior is None else max(prior, round(cpu_pct, 2))
            )
        if mem_limit is not None:
            record["mem_limit_bytes"] = mem_limit

    def refresh_bead(self, record):
        """Look the bead up while it is unknown, a few ticks at most."""
        if record.get("bead") or record.get("bead_given_up"):
            return
        if not record.get("rig") or not record.get("polecat"):
            return
        empty = int(record.get("bead_empty_attempts") or 0)
        errors = int(record.get("bead_error_attempts") or 0)
        if empty >= BEAD_EMPTY_ATTEMPTS or errors >= BEAD_ERROR_ATTEMPTS:
            return
        state, value = self.lookup_bead(record)
        if state == "found":
            record["bead"] = value
            return
        if state == "error":
            record["bead_error_attempts"] = errors + 1
            self.note_once(
                "sandbox %s: bead lookup failed (%s); will retry"
                % (record.get("name"), value)
            )
            if record["bead_error_attempts"] >= BEAD_ERROR_ATTEMPTS:
                record["bead_given_up"] = True
                self.note_once(
                    "sandbox %s: bead unknown after %d failed lookups; peak row "
                    "will carry bead null" % (record.get("name"), BEAD_ERROR_ATTEMPTS)
                )
            return
        record["bead_empty_attempts"] = empty + 1
        if record["bead_empty_attempts"] >= BEAD_EMPTY_ATTEMPTS:
            record["bead_given_up"] = True
            self.note_once(
                "sandbox %s: no hooked or in-progress bead after %d lookups; peak "
                "row will carry bead null" % (record.get("name"), BEAD_EMPTY_ATTEMPTS)
            )

    def finalize_sandbox(self, record, stamp):
        # The last tick that saw the container alive bounds its end; charging
        # a sampler outage to the sandbox would inflate every session that
        # ended during the gap.
        return peak_row(
            bead=record.get("bead"),
            rig=record.get("rig"),
            polecat=record.get("polecat"),
            sandbox=record.get("name"),
            profile=record.get("profile"),
            started_at=record.get("started_at"),
            ended_at=record.get("last_seen") or stamp,
            end_state=None,
            peak_mem_bytes=record.get("peak_mem_bytes"),
            peak_cpu_pct=record.get("peak_cpu_pct"),
            mem_limit_bytes=record.get("mem_limit_bytes"),
            samples=int(record.get("samples") or 0),
            source="docker",
        )

    # -- k8s ------------------------------------------------------------

    def pod_identity(self, pod):
        """(rig, polecat) for a pod named for a session, or None.

        StatefulSets append an ordinal ("gt-imperator-0"); stripping one
        trailing number is enough to recover the session name.
        """
        # The ordinal-stripped name is tried first: SESSION_RE would otherwise
        # read "gt-imperator-0" as the polecat "imperator-0".
        candidates = []
        stripped = re.sub(r"-\d+$", "", pod)
        if stripped != pod:
            candidates.append(stripped)
        candidates.append(pod)
        for candidate in candidates:
            match = SESSION_RE.match(candidate)
            if not match:
                continue
            route = self.prefixes().get(match.group("prefix"))
            if route is not None:
                return route["rig"], match.group("name")
        return None

    def sample_k8s(self, now, interval, stamp):
        kubeconfig = os.environ.get("GT_SAMPLER_KUBECONFIG")
        if not kubeconfig:
            self.note_once("k8s: GT_SAMPLER_KUBECONFIG is not set; source skipped")
            return []
        if shutil.which("kubectl") is None:
            self.note_once("k8s: kubectl is not on PATH; source skipped")
            return []
        code, out, err = self.run(
            ["kubectl", "--kubeconfig", kubeconfig, "top", "pod", "-A", "--no-headers"],
            timeout=20,
        )
        if code != 0:
            self.note_once(
                "k8s: kubectl top pod failed (no metrics API?); source skipped: %s"
                % err.strip()[:120]
            )
            return []
        rows = []
        for line in out.splitlines():
            parts = line.split()
            if len(parts) < 4:
                continue
            _namespace, pod, cpu_text, mem_text = parts[:4]
            component = role_component(pod)
            rig = polecat = None
            if component is None:
                identity = self.pod_identity(pod)
                if identity is None:
                    component = "other"
                else:
                    component = "sandbox"
                    rig, polecat = identity
            rows.append(
                row(
                    ts=stamp,
                    interval_s=interval,
                    source="k8s",
                    **{
                        "class": ROLE_COMPONENT_CLASS.get(component, "other"),
                        "component": component,
                        "name": pod,
                        "rig": rig,
                        "polecat": polecat,
                        "bead": None,
                        "cpu_pct": parse_k8s_cpu(cpu_text),
                        "mem_bytes": parse_size(mem_text),
                        "mem_limit_bytes": None,
                    },
                )
            )
        return rows

    # -- housekeeping ---------------------------------------------------

    def housekeeping(self, now):
        today = now.strftime("%Y-%m-%d")
        if self.state.get("housekeeping_date") == today:
            return
        if os.path.isdir(self.resources_dir):
            for entry in os.scandir(self.resources_dir):
                if not entry.is_file():
                    continue
                name = entry.name
                if name == PEAK_FILENAME or name.startswith(STATE_FILENAME):
                    continue
                try:
                    age = now.timestamp() - entry.stat().st_mtime
                except OSError:
                    continue
                try:
                    if name.endswith(".jsonl") and age > GZIP_AFTER_S:
                        self.gzip_file(entry.path)
                    elif name.endswith(".gz") and age > DELETE_GZ_AFTER_S:
                        os.remove(entry.path)
                except OSError as exc:
                    self.note_once("housekeeping: %s: %s" % (name, exc))
        self.state["housekeeping_date"] = today

    @staticmethod
    def gzip_file(path):
        with open(path, "rb") as source, gzip.open(path + ".gz", "wb") as target:
            shutil.copyfileobj(source, target)
        os.remove(path)


def parse_options(argv):
    parser = argparse.ArgumentParser(
        description="Sample CPU and memory into the metrics resources directory."
    )
    parser.add_argument(
        "--once", action="store_true", help="take one tick and exit"
    )
    parser.add_argument(
        "--now", help="ISO-8601 UTC clock for the tick (tests inject this)"
    )
    parser.add_argument(
        "--interval",
        type=float,
        default=DEFAULT_INTERVAL_S,
        help="seconds between ticks (default %(default)s)",
    )
    parser.add_argument(
        "--metrics-dir",
        default=DEFAULT_METRICS_DIR,
        help="metrics root (default %(default)s)",
    )
    parser.add_argument(
        "--town-root",
        default=None,
        help="town root for routes.jsonl (default GT_TOWN_ROOT or %s)"
        % DEFAULT_TOWN_ROOT,
    )
    options = parser.parse_args(argv)
    if options.interval <= 0:
        parser.error("--interval must be greater than 0")
    if options.now is not None:
        when = parse_iso(options.now)
        if when is None:
            parser.error("--now is not an ISO-8601 time")
        options.now = when
    return options


def main(argv=None):
    options = parse_options(argv if argv is not None else sys.argv[1:])
    sampler = Sampler(options)
    while True:
        started = time.monotonic()
        if options.once:
            # A one-shot run exits non-zero if a bug reaches here: a cron-like
            # caller must be able to see it.
            sampler.tick()
            return 0
        try:
            sampler.tick()
        except Exception as exc:  # an unattended loop outlives one bad tick
            note("tick failed: %s: %s" % (type(exc).__name__, exc))
        delay = options.interval - (time.monotonic() - started)
        if delay > 0:
            time.sleep(delay)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(0)
