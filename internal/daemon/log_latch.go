package daemon

import (
	"fmt"
	"sync"
)

// logLatch logs a line for a key only when it differs from the last line
// logged for that key. The daemon re-checks steady states on every pass (a
// convoy with tracked issues but none ready, a polecat whose crash detection
// is skipped because its work is submitted); logging each pass buried
// everything else in gt tail (~2/3 of daemon lines). With the latch a state
// is logged when it starts or changes, and again if it ends (forget,
// keepOnly) and recurs. The zero value is ready to use.
type logLatch struct {
	mu   sync.Mutex
	last map[string]string
	seen map[string]bool
}

// logf logs format/args through logger unless key's last logged line is the
// same text.
func (l *logLatch) logf(logger func(format string, args ...interface{}), key, format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	l.mu.Lock()
	if l.last == nil {
		l.last = map[string]string{}
	}
	same := l.last[key] == line
	l.last[key] = line
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[key] = true
	l.mu.Unlock()
	if !same {
		logger("%s", line)
	}
}

// forget drops key, so its next line is logged: the state it latched ended.
func (l *logLatch) forget(key string) {
	l.mu.Lock()
	delete(l.last, key)
	l.mu.Unlock()
}

// endPass forgets every key logf did not touch since the last endPass: a
// state the pass no longer reached has ended, so it logs again if it recurs.
// It serves a periodic scan whose keys are only known as it runs; keepOnly
// takes the set up front.
func (l *logLatch) endPass() {
	l.mu.Lock()
	for k := range l.last {
		if !l.seen[k] {
			delete(l.last, k)
		}
	}
	l.seen = nil
	l.mu.Unlock()
}

// keepOnly forgets every key not in keep: those a full scan no longer saw.
func (l *logLatch) keepOnly(keep map[string]bool) {
	l.mu.Lock()
	for k := range l.last {
		if !keep[k] {
			delete(l.last, k)
		}
	}
	l.mu.Unlock()
}
