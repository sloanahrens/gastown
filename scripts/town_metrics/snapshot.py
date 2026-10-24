"""snapshot: one JSON line per run, one column group per module in sources/.

A source module sets GROUP and fills collect(town, run, now), which returns its columns as a dict. A source that
raises becomes a null group plus an errors entry; so does a module that fails to import or has no usable GROUP.
The line is written anyway, so one broken source costs its columns and not the hour. A module added to sources/
is picked up with no other change here.

The append follows internal/jev/battery/log.go: an exclusive flock on the log, mode 0600, and a partial last line
(a short write) stays where it is while the new record starts on its own line. The modes are set on every run, so
a log or directory left looser by an earlier tool is tightened rather than trusted."""
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
    """(group, module) pairs sorted by group, plus one error per module that cannot be used."""
    found, errors = [], []
    for info in pkgutil.iter_modules(package.__path__):
        try:
            mod = importlib.import_module("%s.%s" % (package.__name__, info.name))
        except Exception as e:  # an unimportable module costs its group, not the record
            errors.append(_error(info.name, e))
            continue
        group = getattr(mod, "GROUP", None)
        if not isinstance(group, str) or not callable(getattr(mod, "collect", None)):
            errors.append(_error(info.name, ValueError("%s needs GROUP and collect()" % mod.__name__)))
        elif group in RESERVED or group in {g for g, _ in found}:
            errors.append(_error(info.name, ValueError("%s: group %r is reserved or already taken"
                                                      % (mod.__name__, group))))
        else:
            found.append((group, mod))
    return sorted(found, key=lambda gm: gm[0]), errors


def _error(source, e):
    return {"group": source, "error": "%s: %s" % (type(e).__name__, e)}


def take(town, run, now, package=sources):
    """One record: ts, v, a key per group (null where its source failed), and errors."""
    rec = {"ts": now.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "v": VERSION}
    found, errors = discover(package)
    for group, mod in found:
        try:
            cols = mod.collect(town, run, now)
            if not isinstance(cols, dict):
                raise TypeError("collect returned %s, not a dict" % type(cols).__name__)
            rec[group] = cols
        except Exception as e:  # any source failure is a null group; the line is still written
            rec[group] = None
            errors.append(_error(group, e))
    rec["errors"] = errors
    return rec


def append(path, rec):
    """Append rec as one JSON line under an exclusive flock, in a directory left at 0700 and a log at 0600."""
    directory = os.path.dirname(path)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    os.chmod(directory, 0o700)  # makedirs leaves an existing directory's mode alone
    fd = os.open(path, os.O_RDWR | os.O_APPEND | os.O_CREAT, 0o600)  # read for the tail check, append for the write
    try:
        os.fchmod(fd, 0o600)  # the O_CREAT mode does not apply to a log that already exists
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
