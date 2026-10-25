package beads

import (
	"bytes"
	"context"
	"testing"
)

// A plain call runs in its own process group only when the wrapper asks for
// one, so a gate check killed at its timeout takes bd's children with it, as
// the raw exec it replaced did. Every other plain call keeps CommandWithEnv's
// process group (none added).
func TestPlainCallRunsInItsOwnProcessGroupOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	call := bdCall{bin: "bd", dir: t.TempDir(), args: []string{"gate", "check"}}

	plain := newPlainBDCmd(context.Background(), call, &out, &out)
	if plain.SysProcAttr != nil {
		t.Errorf("plain call has SysProcAttr %+v, want none", plain.SysProcAttr)
	}

	call.group = true
	grouped := newPlainBDCmd(context.Background(), call, &out, &out)
	if grouped.SysProcAttr == nil || !grouped.SysProcAttr.Setpgid {
		t.Errorf("grouped plain call SysProcAttr = %+v, want Setpgid", grouped.SysProcAttr)
	}
}

// WithProcessGroup keeps the wrapper's timeout and reaches its calls, and a
// wrapper that never asks for it has no group (gt-p85pw.8).
func TestWithProcessGroupKeepsTheTimeout(t *testing.T) {
	t.Parallel()
	plain := NewPlain(t.TempDir(), nil).WithTimeout(5)
	if plain.plainGroup {
		t.Errorf("wrapper without WithProcessGroup has plainGroup set")
	}
	grouped := plain.WithProcessGroup()
	if !grouped.plainGroup || grouped.plainTimeout != 5 {
		t.Errorf("WithProcessGroup: plainGroup=%v plainTimeout=%v, want true and 5", grouped.plainGroup, grouped.plainTimeout)
	}
}
