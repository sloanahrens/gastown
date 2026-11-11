package holdclaimtest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExecRefusesACallWithNoCommand(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "exec"}, nil); err == nil {
		t.Fatal("Exec of an argv with no -- and no command returned nil, want an error")
	}
}

func TestExecReturnsTheFailureSetForACommand(t *testing.T) {
	t.Parallel()
	p := New()
	boom := errors.New("pod refused")
	p.Fail["rm"] = boom
	_, err := p.Exec(context.Background(), []string{"kubectl", "--", "rm", "-f", "/x"}, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("Exec error = %v, want the failure the test set", err)
	}
}

func TestExecCatOfAMissingFileFails(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "cat", "/absent"}, nil); err == nil {
		t.Fatal("cat of a file the pod does not hold returned nil, want an error")
	}
}

func TestExecRefusesAnUnknownPodCommand(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "ls", "/"}, nil); err == nil {
		t.Fatal("an unknown pod command returned nil, want an error")
	}
}

func TestExecTeeThenCatReturnsTheWrittenBytes(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "tee", "/f"}, []byte("body")); err != nil {
		t.Fatalf("tee: %v", err)
	}
	got, err := p.Exec(context.Background(), []string{"kubectl", "--", "cat", "/f"}, nil)
	if err != nil {
		t.Fatalf("cat: %v", err)
	}
	if !reflect.DeepEqual(got, []byte("body")) {
		t.Fatalf("cat = %q, want the written bytes", got)
	}
}

func TestExecFindListsTheClaimFileOnlyWhenPresent(t *testing.T) {
	t.Parallel()
	p := New()
	probe := []string{"kubectl", "--", "find", "/claim", "-maxdepth", "1", "-name", "f"}
	out, err := p.Exec(context.Background(), probe, nil)
	if err != nil || len(out) != 0 {
		t.Fatalf("find of a missing file = %q, %v, want an empty listing and exit 0", out, err)
	}
	p.Files["/claim/f"] = []byte("x")
	out, err = p.Exec(context.Background(), probe, nil)
	if err != nil || string(out) != "/claim/f\n" {
		t.Fatalf("find of a present file = %q, %v, want its path and exit 0", out, err)
	}
}

func TestExecRefusesAFindThatIsNotTheReadBack(t *testing.T) {
	t.Parallel()
	p := New()
	for _, argv := range [][]string{
		{"kubectl", "--", "find", "/claim"},
		{"kubectl", "--", "find", "/claim", "-maxdepth", "2", "-name", "f"},
		{"kubectl", "--", "find", "/claim", "-maxdepth", "1", "-type", "f"},
	} {
		if _, err := p.Exec(context.Background(), argv, nil); err == nil || !strings.Contains(err.Error(), "unexpected find call") {
			t.Fatalf("find %q = %v, want the unexpected-call refusal", argv[3:], err)
		}
	}
}

func TestExecFindFailureIsReturnedAsIs(t *testing.T) {
	t.Parallel()
	p := New()
	p.ProbeFail = &ExitError{Code: 1}
	_, err := p.Exec(context.Background(), []string{"kubectl", "--", "find", "/claim", "-maxdepth", "1", "-name", "f"}, nil)
	if !errors.Is(err, p.ProbeFail) {
		t.Fatalf("find error = %v, want the probe failure the test set", err)
	}
}

func TestExecTestDAnswersThatTheMountIsThere(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "test", "-d", "/mount"}, nil); err != nil {
		t.Fatalf("test -d = %v, want exit 0", err)
	}
}

func TestExecRefusesATestThatIsNotTheProbe(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "test", "-f", "/x"}, nil); err == nil {
		t.Fatal("test -f returned nil, want an error")
	}
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "test", "-e", "/f"}, nil); err == nil {
		t.Fatal("test -e with no path returned nil, want an error")
	}
}

func TestExecDownFailsEveryCall(t *testing.T) {
	t.Parallel()
	p := New()
	p.Down = true
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "test", "-d", "/mount"}, nil); err == nil {
		t.Fatal("a down pod answered test -d, want an error")
	}
}

func TestExecRecordsTheContextOfEachCall(t *testing.T) {
	t.Parallel()
	p := New()
	ctx := context.WithValue(context.Background(), struct{}{}, "marker")
	if _, err := p.Exec(ctx, []string{"kubectl", "--", "test", "-d", "/mount"}, nil); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(p.Contexts) != 1 || p.Contexts[0].Value(struct{}{}) != "marker" {
		t.Fatalf("recorded contexts = %v, want the call's own context", p.Contexts)
	}
}
