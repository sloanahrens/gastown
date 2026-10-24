"""snapshot: one JSON line per run, one column group per module in sources/.

A source module sets GROUP and fills collect(town, run, now), which returns its columns as a dict. A source that
raises becomes a null group plus an errors entry; the line is written anyway, so one broken source costs its
columns and not the hour. A module added to sources/ is picked up with no other change here.

The append follows internal/jev/battery/log.go: an exclusive flock on the log, mode 0600, and a partial last line
(a short write) stays where it is while the new record starts on its own line."""
import datetime as dt
import fcntl
import importlib
import json
import os
import pkgutil

from . import sources

VERSION = 1
RESERVED = {"ts", "v", "errors"}


def discover(package=sources):
    """(group, module) for each source module in package, sorted by group. A module without GROUP or collect is an error."""
    found = []
    for info in pkgutil.iter_modules(package.__path__):
        mod = importlib.import_module("%s.%s" % (package.__name__, info.name))
        group = getattr(mod, "GROUP", None)
        if not isinstance(group, str) or not callable(getattr(mod, "collect", None)):
            raise ValueError("%s needs GROUP and collect()" % mod.__name__)
        if group in RESERVED or group in {g for g, _ in found}:
            raise ValueError("%s: group %r is reserved or already taken" % (mod.__name__, group))
        found.append((group, mod))
    return sorted(found, key=lambda gm: gm[0])


def take(town, run, now, package=sources):
    """One record: ts, v, a key per group (null where its source failed), and errors."""
    rec = {"ts": now.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "v": VERSION}
    errors = []
    for group, mod in discover(package):
        try:
            cols = mod.collect(town, run, now)
            if not isinstance(cols, dict):
                raise TypeError("collect returned %s, not a dict" % type(cols).__name__)
            rec[group] = cols
        except Exception as e:  # any source failure is a null group; the line is still written
            rec[group] = None
            errors.append({"group": group, "error": "%s: %s" % (type(e).__name__, e)})
    rec["errors"] = errors
    return rec


def append(path, rec):
    """Append rec as one JSON line under an exclusive flock. A log that ends mid-line gets a newline first."""
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    fd = os.open(path, os.O_RDWR | os.O_APPEND | os.O_CREAT, 0o600)  # read for the tail check, append for the write
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        data = (json.dumps(rec) + "\n").encode()
        if _ends_mid_line(fd):
            data = b"\n" + data
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
    finally:
        os.close(fd)  # closing the descriptor releases the flock


def _ends_mid_line(fd):
    size = os.fstat(fd).st_size
    return size > 0 and os.pread(fd, 1, size - 1) != b"\n"
