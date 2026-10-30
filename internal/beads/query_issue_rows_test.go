package beads

import (
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// queryIssueRows decodes the rows of a bd sql answer whatever envelope the
// installed bd wrapped it in: a bare array (older bd), the plain
// {schema_version, data} wrapper (installed bd, which runs bd sql outside
// machine mode), or the contract document machine mode prints.
func TestQueryIssueRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		out     string
		wantIDs []string
		wantErr string
	}{
		{"bare array", `[{"id":"gt-1"}]`, []string{"gt-1"}, ""},
		{"plain wrapper", `{"data":[{"id":"gt-1"}],"schema_version":1}`, []string{"gt-1"}, ""},
		{"contract document", envelopeArray, []string{"gt-1"}, ""},
		{"two keys that are not the wrapper", `{"data":[{"id":"gt-1"}],"note":"x"}`, nil, "parsing bd sql output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: tt.out} })
			b := newRecordedBeads(t.TempDir(), rec)

			rows, err := b.queryIssueRows(beadsql.WispsWithLabels([]string{"gt:merge-request"}))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("queryIssueRows err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("queryIssueRows: %v", err)
			}
			var ids []string
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Errorf("queryIssueRows ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

// Beads.run unwraps the plain wrapper only for the machine-exempt sql verb. A
// two-key object that is not the wrapper, and any other verb's plain wrapper,
// reach the caller untouched.
func TestRunUnwrapPlainEnvelope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		stdout string
		want   string
	}{
		{
			name:   "sql unwraps the plain wrapper",
			args:   []string{"sql", "--json", "SELECT id FROM issues"},
			stdout: `{"data":[{"id":"gt-1"}],"schema_version":1}`,
			want:   `[{"id":"gt-1"}]`,
		},
		{
			name:   "sql passes a two-key object that is not the wrapper",
			args:   []string{"sql", "--json", "SELECT id FROM issues"},
			stdout: `{"data":[{"id":"gt-1"}],"note":"x"}`,
			want:   `{"data":[{"id":"gt-1"}],"note":"x"}`,
		},
		{
			name:   "show does not unwrap the plain wrapper",
			args:   []string{"show", "gt-1", "--json"},
			stdout: `{"data":[{"id":"gt-1"}],"schema_version":1}`,
			want:   `{"data":[{"id":"gt-1"}],"schema_version":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(func([]string) reply { return reply{stdout: tt.stdout} })
			b := newRecordedBeads(t.TempDir(), rec)

			out, err := b.run(tt.args...)
			if err != nil || string(out) != tt.want {
				t.Fatalf("run %v = %q, %v; want %q", tt.args, out, err, tt.want)
			}
		})
	}
}
