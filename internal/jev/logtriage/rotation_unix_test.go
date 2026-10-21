//go:build unix

package logtriage

import (
	"os"
	"testing"
)

// TestJevLogtriageCursorSurvivesRotation: a rotated daemon log is a new file,
// so the next pass reads its first line and does not read past the old one's
// offset. Rotation is detected by inode, which only unix platforms read.
func TestJevLogtriageCursorSurvivesRotation(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineTick1, lineWarn)
	fx.run()

	if err := os.Rename(fx.pass.DaemonLog, fx.pass.DaemonLog+".1"); err != nil {
		t.Fatal(err)
	}
	// The new file is longer than the old cursor's offset, so only the inode
	// change, not the size check, tells the pass to start over.
	fx.logLines(lineMerge1, lineMerge2, lineTick2)
	fx.jev.requests = nil

	sum := fx.run()
	if sum.Matched != 3 || sum.Asked != 2 {
		t.Errorf("pass after rotation = %+v, want the new file's three lines in two groups", sum)
	}
	lines := fx.triage()
	if len(lines) != 4 || lines[3].Key != keyTick || lines[3].Count != 1 {
		t.Errorf("triage log = %+v, want the new file's groups appended", lines)
	}
}
