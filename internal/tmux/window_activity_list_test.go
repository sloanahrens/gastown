package tmux

import (
	"testing"
	"time"
)

func TestParseWindowActivityList(t *testing.T) {
	t.Parallel()
	got := parseWindowActivityList("1759500000 gt-agate\n1759500060 hq-mayor\nbogus line\n  \n12 \nxx gt-bad\n")
	if len(got) != 2 {
		t.Fatalf("parsed %d sessions, want 2: %v", len(got), got)
	}
	if !got["gt-agate"].Equal(time.Unix(1759500000, 0)) || !got["hq-mayor"].Equal(time.Unix(1759500060, 0)) {
		t.Errorf("times wrong: %v", got)
	}
}
