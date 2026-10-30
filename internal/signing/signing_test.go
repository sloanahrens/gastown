package signing

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	errRefused     = errors.New("unlock refused")
	errRestoreFail = errors.New("restore refused")
)

// fakeExec records each command and answers it with the stdout, stderr and error
// the test sets. A codesign answers with codesignErr; security answers with unlockErr.
type fakeExec struct {
	calls       [][]string
	stdins      []string
	stdout      string
	stderr      string
	unlockErr   error
	codesignErr error
}

func (f *fakeExec) exec(_ context.Context, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "security" {
		return f.stdout, f.stderr, f.unlockErr
	}
	return f.stdout, f.stderr, f.codesignErr
}

// execStdin is the stdin seam Unlock runs through. It records the bytes fed to
// the command's standard input next to its argv, and answers with unlockErr.
func (f *fakeExec) execStdin(_ context.Context, stdin, name string, args ...string) (string, string, error) {
	f.stdins = append(f.stdins, stdin)
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.stdout, f.stderr, f.unlockErr
}

func TestUnlockReportsATimeoutOnlyWhenTheDeadlineCutItOff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// now and limit bound the unlock: a deadline already in the past is one the
		// unlock ran into, and a deadline far ahead is one it did not reach.
		now   time.Time
		limit time.Duration
		exec  *fakeExec
		want  error
	}{
		{
			name:  "deadline already passed and the command failed",
			now:   time.Now().Add(-time.Hour),
			limit: time.Minute,
			exec:  &fakeExec{unlockErr: errRefused},
			want:  ErrUnlockTimeout,
		},
		{
			name:  "deadline ahead and the command refused",
			now:   time.Now(),
			limit: time.Hour,
			exec:  &fakeExec{unlockErr: errRefused},
			want:  errRefused,
		},
		{
			name:  "deadline ahead and the unlock succeeded",
			now:   time.Now(),
			limit: time.Hour,
			exec:  &fakeExec{},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := Bound(context.Background(), func() time.Time { return tc.now }, tc.limit)
			defer cancel()
			err := Unlock(ctx, tc.exec.execStdin, "/keys/gt.keychain-db", "pass")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Unlock() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUnlockKeepsThePasswordOffArgvAndOnStdin pins the contract: the argv is
// security unlock-keychain and the keychain, with no -p and nothing that holds
// the password, and stdin is the password plus one newline.
func TestUnlockKeepsThePasswordOffArgvAndOnStdin(t *testing.T) {
	t.Parallel()
	const password = "s3cret-pass"
	exec := &fakeExec{}
	if err := Unlock(context.Background(), exec.execStdin, "/keys/gt.keychain-db", password); err != nil {
		t.Fatalf("Unlock() = %v, want nil", err)
	}
	want := []string{"security", "unlock-keychain", "/keys/gt.keychain-db"}
	if len(exec.calls) != 1 || !reflect.DeepEqual(exec.calls[0], want) {
		t.Errorf("command = %q, want %q", exec.calls, want)
	}
	for _, arg := range exec.calls[0] {
		if strings.Contains(arg, password) {
			t.Errorf("argv %q carries the password", exec.calls[0])
		}
	}
	if want := []string{password + "\n"}; !reflect.DeepEqual(exec.stdins, want) {
		t.Errorf("stdin = %q, want %q", exec.stdins, want)
	}
}

func TestUnlockPassesThePathAsAnOptionSafeOperand(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	if err := Unlock(context.Background(), exec.execStdin, "-keychain", "pass"); err != nil {
		t.Fatalf("Unlock() = %v, want nil", err)
	}
	want := []string{"security", "unlock-keychain", "./-keychain"}
	if got := exec.calls[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
}

// TestUnlockErrorNeverCarriesThePassword: a refused unlock returns an error that
// names no password, even when the command's output names the keychain, and a
// deadline that cut the unlock off still reads as ErrUnlockTimeout.
func TestUnlockErrorNeverCarriesThePassword(t *testing.T) {
	t.Parallel()
	const password = "s3cret-pass"
	cases := []struct {
		name    string
		now     time.Time
		limit   time.Duration
		wantErr error
	}{
		{name: "refused before the deadline", now: time.Now(), limit: time.Hour, wantErr: errRefused},
		{name: "refused after the deadline", now: time.Now().Add(-time.Hour), limit: time.Minute, wantErr: ErrUnlockTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := &fakeExec{unlockErr: errRefused, stderr: "SecKeychainUnlock /keys/gt.keychain-db: wrong passphrase"}
			ctx, cancel := Bound(context.Background(), func() time.Time { return tc.now }, tc.limit)
			defer cancel()
			err := Unlock(ctx, exec.execStdin, "/keys/gt.keychain-db", password)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Unlock() = %v, want %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), password) {
				t.Errorf("Unlock() = %q carries the password", err)
			}
		})
	}
}

// TestCodesignGuardsALeadingDashInKeychainAndPath pins the argv Codesign hands
// exec: a dash-leading keychain or path is read from ./, never as an option.
func TestCodesignGuardsALeadingDashInKeychainAndPath(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	err := Codesign(context.Background(), exec.exec, "-keys/k", "id", "com.gastown.gt", "-build", func() error { return nil })
	if err != nil {
		t.Fatalf("Codesign() = %v, want nil", err)
	}
	want := []string{"codesign", "--force", "--keychain", "./-keys/k", "-s", "id", "-i", "com.gastown.gt", "./-build"}
	if len(exec.calls) != 1 || !reflect.DeepEqual(exec.calls[0], want) {
		t.Errorf("codesign argv = %q, want %q", exec.calls, want)
	}
}

func TestCodesignRestoresTheBuildAndNamesTheFirstLineOfItsOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		stdout     string
		stderr     string
		wantNotice string
	}{
		{name: "stderr first line", stderr: "bad identity\nmore\n", wantNotice: "bad identity"},
		{name: "stdout when stderr is empty", stdout: "no identity\n", wantNotice: "no identity"},
		{name: "no output at all", wantNotice: "no output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := &fakeExec{stdout: tc.stdout, stderr: tc.stderr, codesignErr: errRefused}
			restored := 0
			err := Codesign(context.Background(), exec.exec, "/keys/k", "id", "com.gastown.gt", "/tmp/build", func() error {
				restored++
				return nil
			})
			var ce CodesignError
			if !errors.As(err, &ce) {
				t.Fatalf("Codesign() = %v, want a CodesignError", err)
			}
			if ce.Notice != tc.wantNotice {
				t.Errorf("Notice = %q, want %q", ce.Notice, tc.wantNotice)
			}
			if restored != 1 {
				t.Errorf("restore ran %d times, want 1 before the build is judged", restored)
			}
		})
	}
}

func TestCodesignReturnsARestoreFailureAsItIs(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{codesignErr: errRefused}
	err := Codesign(context.Background(), exec.exec, "/keys/k", "id", "com.gastown.gt", "/tmp/build", func() error {
		return errRestoreFail
	})
	if !errors.Is(err, errRestoreFail) {
		t.Fatalf("Codesign() = %v, want the restore failure", err)
	}
	var ce CodesignError
	if errors.As(err, &ce) {
		t.Errorf("Codesign() = %v, a restore failure must not read as a codesign failure", err)
	}
}

func TestCodesignSuccessLeavesTheSignedBuildAndSkipsRestore(t *testing.T) {
	t.Parallel()
	exec := &fakeExec{}
	restored := false
	err := Codesign(context.Background(), exec.exec, "/keys/k", "id", "com.gastown.gt", "/tmp/build", func() error {
		restored = true
		return nil
	})
	if err != nil {
		t.Fatalf("Codesign() = %v, want nil", err)
	}
	if restored {
		t.Error("restore ran after a successful codesign")
	}
	want := []string{"codesign", "--force", "--keychain", "/keys/k", "-s", "id", "-i", "com.gastown.gt", "/tmp/build"}
	if got := exec.calls[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
}

func TestOptionSafeKeepsADashPathFromReadingAsAnOption(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"-i":              "./-i",
		"/usr/local/gt":   "/usr/local/gt",
		"bin/gt":          "bin/gt",
		"./-already-safe": "./-already-safe",
	} {
		if got := OptionSafe(in); got != want {
			t.Errorf("OptionSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
