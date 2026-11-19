import sys

p = 'docs/design/pod-engine.md'
s = open(p).read()
old = 'gated internal/daemon/ci_engine_prune.go:723 when'
new = 'gated internal/daemon/ci_engine_prune.go:754 when'
count = s.count(old)
if count != 6:
    sys.exit('expected 6 gate refs, found %d' % count)
open(p, 'w').write(s.replace(old, new))
print('rewrote %d gate refs' % count)
