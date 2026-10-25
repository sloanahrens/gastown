//go:build unix

package spans

import (
	"path/filepath"
	"testing"
)

// The in-process mutex map holds only the logs in use: once the last holder of
// a log's lock releases it, nothing remains for that path (gt-rm0ab).
func TestLockLog_ForgetsAPathOnceNoOneHoldsIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gt-abc.jsonl.lock")
	first, err := lockLog(path)
	if err != nil {
		t.Fatalf("lockLog: %v", err)
	}
	first.unlock()

	logMutexesMu.Lock()
	_, kept := logMutexes[path]
	logMutexesMu.Unlock()
	if kept {
		t.Errorf("the mutex for %s is still in the map after its last unlock", path)
	}
}
