package plugin

import (
	"strings"
	"testing"
)

func TestFormatFailureMailBody(t *testing.T) {
	p := &Plugin{Name: "dolt-backup", Description: "backs up", Path: "/town/plugins/dolt-backup", HasRunScript: true}
	body := p.FormatFailureMailBody("exit 7 after 3s", "line one\nthe disk is full\n")
	for _, want := range []string{"## Direct run failed", "exit 7 after 3s", "the disk is full", "Rerun the script only if the failure looks transient", "cd /town/plugins/dolt-backup && bash run.sh", "gt dog done"} {
		if !strings.Contains(body, want) {
			t.Errorf("failure body lacks %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "Direct run failed") > strings.Index(body, "bash run.sh") {
		t.Error("the failure section must come before the ordinary instructions")
	}
}
