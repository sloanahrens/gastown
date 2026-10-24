"""Per-landing diff statistics from `git show --numstat`. Pure parsing plus one thin git source."""
import os
import re

from .common import entropy

_BRACE = re.compile(r"\{([^{}]*) => ([^{}]*)\}")


def _resolve_path(p):
    if "{" in p:
        p = _BRACE.sub(lambda m: m.group(2), p).replace("//", "/")
    elif " => " in p:
        p = p.split(" => ", 1)[1]
    return p


def parse_numstat(text):
    out = []
    for line in text.splitlines():
        parts = line.split("\t", 2)
        if len(parts) != 3:
            continue
        a, d, path = parts
        out.append((0 if a == "-" else int(a), 0 if d == "-" else int(d), _resolve_path(path)))
    return out


def diff_stats(entries):
    files, pkgs = {}, {}
    for a, d, path in entries:
        w = max(a + d, 1)
        files[path] = files.get(path, 0) + w
        pkg = os.path.dirname(path) or "."
        pkgs[pkg] = pkgs.get(pkg, 0) + w
    return {
        "lines_added": sum(e[0] for e in entries),
        "lines_deleted": sum(e[1] for e in entries),
        "files_touched": len(files),
        "file_entropy": entropy(files.values()),
        "package_entropy": entropy(pkgs.values()),
    }


class GitDiffSource:
    """bare_repo_for(rig) -> path; run(args) -> stdout or None on failure; cache maps (rig, commit) -> stats."""

    def __init__(self, bare_repo_for, run, cache):
        self._repo, self._run, self.cache = bare_repo_for, run, cache

    def stats(self, rig, commit):
        key = (rig, commit)
        if key in self.cache:
            return self.cache[key]
        out = self._run(["--git-dir", self._repo(rig), "show", "--numstat", "-M", "--format=", commit])
        if out is None:
            return None
        s = diff_stats(parse_numstat(out))
        self.cache[key] = s
        return s
