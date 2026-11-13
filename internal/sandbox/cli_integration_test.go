//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestIntegrationExistsThroughExecRunner reads the not-found status from the
// stderr the real exec runner returns with a failed process, so TestExists
// does not rest on a Fake that hands stderr over alongside the error.
func TestIntegrationExistsThroughExecRunner(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		want    bool
		wantErr bool
	}{
		{name: "not_found", stderr: `Error:   × code: 'Some requested entity was not found', message: "sandbox not found"`},
		{name: "gateway_unreachable", stderr: "transport error: connection refused", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A shell prints the stderr and fails, as openshell does. The
			// message rides as $1, not env, so env redaction has nothing to
			// match.
			run := func(ctx context.Context, _ []string, stdin []byte, env []string) ([]byte, []byte, error) {
				return execRunner(ctx, []string{"sh", "-c", `printf %s "$1" >&2; exit 1`, "sh", tt.stderr}, stdin, env)
			}
			got, err := (&CLI{Run: run}).Exists(context.Background(), "x")
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("Exists = %v, %v; want %v (error: %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestIntegrationSandboxCLIList runs List through the real exec runner against
// a stub openshell that prints the captured pages, so the argv, the paging
// token and the JSON reading run once with no gateway. The stub is written
// under t.TempDir and named openshell.
func TestIntegrationSandboxCLIList(t *testing.T) {
	page1, err := filepath.Abs("testdata/sandbox_list_page1.json")
	if err != nil {
		t.Fatal(err)
	}
	page2, err := filepath.Abs("testdata/sandbox_list_page2.json")
	if err != nil {
		t.Fatal(err)
	}
	raw1, err := os.ReadFile(page1)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := parseListPage(raw1)
	if err != nil || token == "" {
		t.Fatalf("page one has no token to page on: %q, %v", token, err)
	}
	// The stub prints page two for the token page one returned, page one for
	// the first call, and fails for any other token.
	stub := filepath.Join(t.TempDir(), "openshell")
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *" --page-token %s"*) cat '%s' ;;
  *" --page-token "*) echo "unknown page token" >&2; exit 1 ;;
  *) cat '%s' ;;
esac
`, token, page2, page1)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := (&CLI{Gateway: "k8s", Bin: stub}).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Entry{{Name: "gt-dementus", Phase: "Ready"}, {Name: "gt-furiosa", Phase: "Ready"}, {Name: "gt-slit", Phase: "Ready"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %v, want %v", got, want)
	}
}
