package sbxcreds

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// lineFormat records the subject and not-after it is given, then returns line or err.
type lineFormat struct {
	line, subject string
	err           error
	notAfter      time.Time
	calls         int
}

func (f *lineFormat) format(subject string, notAfter time.Time) (string, error) {
	f.calls++
	f.subject, f.notAfter = subject, notAfter
	return f.line, f.err
}

// failingWriter refuses every write, as a closed pipe does.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("the pipe is closed") }

// TestIssueLinePrintsLine asserts an issued request writes the kubeconfig to path only, gives the
// subject and not-after to format, and prints its line followed by a newline.
func TestIssueLinePrintsLine(t *testing.T) {
	t.Parallel()
	dir := testutil.PrivateTempDir(t)
	path := filepath.Join(dir, "deployer.kubeconfig")
	doc := kubeconfigDoc(testCertData(t))
	f := &lineFormat{line: "gt-deployer-apply expires 2027-01-01"}
	var out bytes.Buffer
	if err := IssueLine(context.Background(), (&fakeRunner{out: doc}).run, issueRequest, path, f.format, &out); err != nil {
		t.Fatalf("IssueLine() = %v, want the line printed", err)
	}
	if want, wantTime := "CN=gt-deployer-apply", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); f.subject != want || !f.notAfter.Equal(wantTime) {
		t.Errorf("format got %q, %v, want %q, %v", f.subject, f.notAfter, want, wantTime)
	}
	if out.String() != f.line+"\n" {
		t.Errorf("printed %q, want %q", out.String(), f.line+"\n")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(doc) {
		t.Errorf("kubeconfig at %s = %q (read %v), want the printed kubeconfig", path, got, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("%s holds %d entries (read %v), want only the kubeconfig", dir, len(entries), err)
	}
}

// TestIssueLineIssueFailure drives each Issue refusal: the error is returned, format is not called,
// nothing is printed and path is left as it was.
func TestIssueLineIssueFailure(t *testing.T) {
	t.Parallel()
	doc := kubeconfigDoc(testCertData(t))
	badRequest := issueRequest
	badRequest.Validity = "soon"
	for _, tc := range []struct {
		name, want string
		req        Request
		runErr     error
		existing   bool
	}{
		{"invalid request", "is not a duration", badRequest, nil, false},
		{"runner error", "printed no kubeconfig", issueRequest, errors.New("exit status 1"), false},
		{"path already exists", "could not write the kubeconfig", issueRequest, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(testutil.PrivateTempDir(t), "deployer.kubeconfig")
			if tc.existing {
				if err := os.WriteFile(path, []byte(plantedContent), 0o600); err != nil {
					t.Fatalf("plant %s: %v", path, err)
				}
			}
			f := &lineFormat{line: "unused"}
			var out bytes.Buffer
			err := IssueLine(context.Background(), (&fakeRunner{out: doc, err: tc.runErr}).run, tc.req, path, f.format, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("IssueLine() = %v, want a refusal naming %q", err, tc.want)
			}
			if f.calls != 0 || out.Len() != 0 {
				t.Errorf("format called %d times and printed %q after an Issue refusal, want neither", f.calls, out.String())
			}
			got, readErr := os.ReadFile(path)
			if tc.existing && (readErr != nil || string(got) != plantedContent) {
				t.Errorf("%s = %q (read %v), want the existing file untouched", path, got, readErr)
			}
			if !tc.existing && !errors.Is(readErr, os.ErrNotExist) {
				t.Errorf("%s holds %q (read %v) after the refusal, want nothing written", path, got, readErr)
			}
		})
	}
}

// TestIssueLineFormatFailure asserts a format error is returned and nothing is printed.
func TestIssueLineFormatFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(testutil.PrivateTempDir(t), "deployer.kubeconfig")
	f := &lineFormat{err: errors.New("the line template is missing")}
	var out bytes.Buffer
	err := IssueLine(context.Background(), (&fakeRunner{out: kubeconfigDoc(testCertData(t))}).run, issueRequest, path, f.format, &out)
	if err == nil || !strings.Contains(err.Error(), "the line template is missing") {
		t.Fatalf("IssueLine() = %v, want format's refusal", err)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q after a format refusal, want nothing", out.String())
	}
}

// TestIssueLineWriteFailure asserts a failed print is returned, not swallowed.
func TestIssueLineWriteFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(testutil.PrivateTempDir(t), "deployer.kubeconfig")
	f := &lineFormat{line: "gt-deployer-apply expires 2027-01-01"}
	err := IssueLine(context.Background(), (&fakeRunner{out: kubeconfigDoc(testCertData(t))}).run, issueRequest, path, f.format, failingWriter{})
	if err == nil || !strings.Contains(err.Error(), "could not print the line") {
		t.Fatalf("IssueLine() = %v, want the print failure returned", err)
	}
}
