package holdclaimtest

import (
	"context"
	"errors"
	"reflect"
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
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "sh", "-c", "x"}, nil); err == nil {
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

func TestExecProbeAnswersWhetherTheClaimFileIsPresent(t *testing.T) {
	t.Parallel()
	p := New()
	probe := []string{"kubectl", "--", "sh", "-c", "script", "sh", "/f"}
	got, err := p.Exec(context.Background(), probe, nil)
	if err != nil || string(got) != "absent\n" {
		t.Fatalf("probe of a missing file = %q, %v; want absent", got, err)
	}
	p.Files["/f"] = []byte("x")
	got, err = p.Exec(context.Background(), probe, nil)
	if err != nil || string(got) != "present\n" {
		t.Fatalf("probe of a present file = %q, %v; want present", got, err)
	}
}

func TestExecRefusesAnShCallThatIsNotTheProbe(t *testing.T) {
	t.Parallel()
	p := New()
	if _, err := p.Exec(context.Background(), []string{"kubectl", "--", "sh", "-c", "x"}, nil); err == nil {
		t.Fatal("an sh call with no path argument returned nil, want an error")
	}
}
