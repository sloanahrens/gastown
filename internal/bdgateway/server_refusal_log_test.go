package bdgateway

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestServeRefusalPreAuthIsBounded: 50 unauthenticated requests in one window
// write the window's 10 refusal lines and no more, yet GET /v1/stats counts all
// 50; a 403 after authentication always writes its line (gt-ahqbh6).
func TestServeRefusalPreAuthIsBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := newTestCallLogWithDir(t, dir)
	g := newTestGateway(t, mustStore(t), log, &stubRunner{})
	now := time.Now()
	g.preAuth.now = func() time.Time { return now }
	head := Request{Argv: []string{"show", "gt-x"}}
	for i := 0; i < 50; i++ {
		g.ServeHTTP(httptest.NewRecorder(), execRequest(t, "not-a-token", head, ""))
	}
	forbidden := Request{Argv: []string{"show", "--json", "--include-comments", "--db"}}
	g.ServeHTTP(httptest.NewRecorder(), execRequest(t, testReadToken, forbidden, ""))

	// Past the window, the next refusal first writes the dropped-count line.
	now = now.Add(2 * refusalWindowLength)
	g.ServeHTTP(httptest.NewRecorder(), execRequest(t, "not-a-token", head, ""))

	stats := log.Stats()
	if stats.Calls != 52 || stats.Refusals[codeUnauthorized] != 51 || stats.Refusals[codeForbidden] != 1 {
		t.Errorf("stats = calls %d, refusals %v, want 52 calls, 51 unauthorized and 1 forbidden", stats.Calls, stats.Refusals)
	}
	var unauthorized, forbiddenLines, suppressedLines int
	for _, line := range closedCallLines(t, log, dir) {
		e := decodeEntry(t, line)
		switch e.Refusal {
		case codeRefusalsSuppressed:
			suppressedLines++
			if n, _ := strconv.Atoi(strings.Fields(e.Error)[0]); n != 40 {
				t.Errorf("suppressed line = %q, want its leading count to be 40", e.Error)
			}
		case codeUnauthorized:
			unauthorized++
		case codeForbidden:
			forbiddenLines++
		}
	}
	if unauthorized != refusalLinesPerWindow+1 || forbiddenLines != 1 || suppressedLines != 1 {
		t.Errorf("log holds %d unauthorized, %d forbidden and %d suppressed lines, want %d, 1 and 1", unauthorized, forbiddenLines, suppressedLines, refusalLinesPerWindow+1)
	}
}
