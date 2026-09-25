package convoy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFeedNextReadyIssue_OperatorHold_DispatchesNothing is gt-ifijm on the
// event-driven convoy feed: while the operator's town-wide hold file exists,
// a close event must not sling the convoy's next issue. The hold is answered
// before the store is read, so a nil store proves nothing else ran.
func TestFeedNextReadyIssue_OperatorHold_DispatchesNothing(t *testing.T) {
	townRoot := setupTownRoot(t)
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatalf("write hold: %v", err)
	}
	gtPath, logPath := makeGTStub(t, 0)
	logger, logMsgs := makeLogger()

	feedNextReadyIssue(context.Background(), nil, townRoot, "hq-cv-hold", "test", logger, gtPath, func(string) bool { return false }, nil)

	if data, err := os.ReadFile(logPath); err == nil {
		t.Errorf("gt was invoked during an operator hold: %s", data)
	}
	found := false
	for _, m := range *logMsgs {
		if strings.Contains(m, "hq-cv-hold") && strings.Contains(m, "seat-refill.hold") {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line naming the hold for convoy hq-cv-hold; got %v", *logMsgs)
	}
}
