import sys

deps = {}
pkg = None
with open(sys.argv[1]) as f:
    for line in f:
        parts = line.split()
        if len(parts) == 2:
            pkg = parts[0]
            deps[pkg] = set()
        elif len(parts) == 1 and pkg:
            deps[pkg].add(parts[0])
tutil = 'github.com/steveyegge/gastown/internal/testutil'
hits = [p for p, d in deps.items() if tutil in d and '/internal/' in p]
for h in sorted(hits):
    print(h)
