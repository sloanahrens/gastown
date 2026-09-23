"""Temporarily drop the once-per-run guard to prove the count assertion catches it."""
import sys

p = "plugins/rebuild-gt/run.sh"
s = open(p).read()
old = """  if [ "$NOTE_BLOCKED" = "1" ]; then
    log "Still blocked: $1"
    return 0
  fi
  NOTE_BLOCKED=1
"""
new = """  # MUTATION: no once-per-run guard
"""
if sys.argv[1] == "apply":
    assert old in s, "anchor not found"
    open(p, "w").write(s.replace(old, new))
else:
    assert new in s, "mutation not present"
    open(p, "w").write(s.replace(new, old))
print("ok")
