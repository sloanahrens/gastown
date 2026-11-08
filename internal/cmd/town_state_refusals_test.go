package cmd

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestTownStateFlagReadsRefuseAnUndefinedFlag drives the flag reads of the
// export and import commands: a flag a command does not define is an error
// that names it, never an empty value read as the default.
func TestTownStateFlagReadsRefuseAnUndefinedFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		run     func(*cobra.Command) error
		defined []string
		want    string
	}{
		{name: "export without --town", run: func(c *cobra.Command) error { return runTownStateExport(c, nil) }, want: "reading --town"},
		{name: "export without --since", run: func(c *cobra.Command) error { return runTownStateExport(c, nil) }, defined: []string{"town"}, want: "reading --since"},
		{name: "export without --rollback", run: func(c *cobra.Command) error { return runTownStateExport(c, nil) }, defined: []string{"town", "since"}, want: "reading --rollback"},
		{name: "import without --town", run: func(c *cobra.Command) error { return runTownStateImport(c, nil) }, want: "reading --town"},
		{name: "import without --merge-newer", run: func(c *cobra.Command) error { return runTownStateImport(c, nil) }, defined: []string{"town"}, want: "reading --merge-newer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range tc.defined {
				cmd.Flags().String(name, "", "")
			}
			err := tc.run(cmd)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestTownStateExportReportsAFailedExport: an export of a town root that does
// not exist fails and names the export, so the caller sees a non-zero exit.
func TestTownStateExportReportsAFailedExport(t *testing.T) {
	t.Parallel()
	err := runTownStateExportIn(townStateExportEnv{
		town:    filepath.Join(t.TempDir(), "missing"),
		out:     io.Discard,
		summary: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "exporting the town state") {
		t.Fatalf("export of a missing town root = %v, want the export refusal", err)
	}
}
