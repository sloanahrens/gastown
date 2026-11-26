package sbxcreds

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// issueRequest is a valid request for client gt-deployer-apply in the control container.
var issueRequest = Request{Container: "gt-sbx-control", ClientName: "gt-deployer-apply", Group: "gt-deployers", Validity: "24h"}

// fakeRunner stands in for the docker exec runner: it records the argv it is given and returns a canned output and error.
type fakeRunner struct {
	out   []byte
	err   error
	calls int
	argv  []string
}

func (f *fakeRunner) run(_ context.Context, argv []string) ([]byte, error) {
	f.calls++
	f.argv = argv
	return f.out, f.err
}

// TestIssueReturnsSubjectAndExpiry asserts Issue runs the request's argv, returns the certificate's
// subject and not-after, and writes the printed kubeconfig.
func TestIssueReturnsSubjectAndExpiry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(testutil.PrivateTempDir(t), "deployer.kubeconfig")
	doc := kubeconfigDoc(testCertData(t))
	r := &fakeRunner{out: doc}
	subject, notAfter, err := Issue(context.Background(), r.run, issueRequest, path)
	if err != nil {
		t.Fatalf("Issue() = %v, want the certificate's subject and not-after", err)
	}
	if want, wantTime := "CN=gt-deployer-apply", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); subject != want || !notAfter.Equal(wantTime) {
		t.Errorf("Issue() = %q, %v, want %q, %v", subject, notAfter, want, wantTime)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(doc) {
		t.Errorf("kubeconfig at %s = %q (read %v), want the printed kubeconfig", path, got, err)
	}
	want, err := issueRequest.Argv()
	if err != nil {
		t.Fatalf("Argv() = %v", err)
	}
	if r.calls != 1 || !reflect.DeepEqual(r.argv, want) {
		t.Errorf("runner called %d times with %q, want once with %q", r.calls, r.argv, want)
	}
}

// TestIssueFails drives every refusal. Each row must return an error naming its cause, zero values,
// no key marker in the error or the log, and leave the path as it was: nothing written, or an
// existing file untouched.
//
//testpolicy:allow parallel — captures the process-wide standard log output, so no other test may run beside this one
func TestIssueFails(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	doc := kubeconfigDoc(testCertData(t))
	badRequest := issueRequest
	badRequest.Validity = "soon"
	for _, tc := range []struct {
		name, want string
		req        Request
		out        []byte
		runErr     error
		existing   bool
		runs       int
	}{
		{"invalid request", "is not a duration", badRequest, doc, nil, false, 0},
		{"runner error", "printed no kubeconfig", issueRequest, []byte(keyMarker), errors.New("exit status 1: " + keyMarker), false, 1},
		{"no client certificate", "no client-certificate-data", issueRequest, kubeconfigDoc(""), nil, false, 1},
		{"not a kubeconfig", "not a kubeconfig", issueRequest, []byte("users: [" + keyMarker), nil, false, 1},
		{"path already exists", "could not write the kubeconfig", issueRequest, doc, nil, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			path := filepath.Join(testutil.PrivateTempDir(t), "deployer.kubeconfig")
			if tc.existing {
				if err := os.WriteFile(path, []byte(plantedContent), 0o600); err != nil {
					t.Fatalf("plant %s: %v", path, err)
				}
			}
			r := &fakeRunner{out: tc.out, err: tc.runErr}
			subject, notAfter, err := Issue(context.Background(), r.run, tc.req, path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Issue() = %v, want a refusal naming %q", err, tc.want)
			}
			if strings.Contains(err.Error(), keyMarker) || strings.Contains(logs.String(), keyMarker) {
				t.Errorf("Issue() error %q or log %q echoes the key marker", err, logs.String())
			}
			if subject != "" || !notAfter.IsZero() {
				t.Errorf("Issue() returned %q, %v with the refusal, want zero values", subject, notAfter)
			}
			if r.calls != tc.runs {
				t.Errorf("runner called %d times, want %d", r.calls, tc.runs)
			}
			got, readErr := os.ReadFile(path)
			if tc.existing {
				if readErr != nil || string(got) != plantedContent {
					t.Errorf("%s = %q (read %v), want the existing file untouched", path, got, readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Errorf("%s holds %q (read %v) after the refusal, want nothing written", path, got, readErr)
			}
		})
	}
}
