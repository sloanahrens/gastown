package overlap

import (
	"context"
	"strings"
	"testing"
)

func TestRealRunStep_SuccessAndFailure(t *testing.T) {
	run := RealRunStep(t.TempDir())

	ok := run(context.Background(), SuiteStep{Name: "ok", Cmd: "echo hello"})
	if !ok.Success || !strings.Contains(ok.Output, "hello") {
		t.Fatalf("ok = %+v, want Success with output containing 'hello'", ok)
	}

	bad := run(context.Background(), SuiteStep{Name: "bad", Cmd: "exit 3"})
	if bad.Success || bad.ExitCode != 3 {
		t.Fatalf("bad = %+v, want Success=false ExitCode=3", bad)
	}
}

func TestRealRunStep_CancelledContextIsNotSuccess(t *testing.T) {
	run := RealRunStep(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := run(ctx, SuiteStep{Name: "canceled", Cmd: "echo should-not-matter"})
	if result.Success {
		t.Fatalf("result = %+v, want Success=false for an already-canceled context", result)
	}
}

func TestCapOutput(t *testing.T) {
	short := "hello"
	if got := capOutput(short); got != short {
		t.Fatalf("capOutput(short) = %q, want unchanged", got)
	}

	long := strings.Repeat("x", maxCapturedOutput+100)
	got := capOutput(long)
	if len(got) <= maxCapturedOutput || len(got) >= len(long) {
		t.Fatalf("capOutput did not shrink a %d-byte input (got %d bytes)", len(long), len(got))
	}
	if !strings.HasPrefix(got, "...[truncated]...") {
		t.Fatalf("capOutput(long) = %q, want a truncation marker", got[:40])
	}
	if !strings.HasSuffix(got, long[len(long)-10:]) {
		t.Fatal("capOutput did not keep the tail of the original output")
	}
}
