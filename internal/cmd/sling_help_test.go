package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// TestSlingHelpNamesNoRetiredRole pins gt-rj21p: gt sling never targets the
// witness or refinery (both retired), so its help must not offer them as
// agents to sling to.
func TestSlingHelpNamesNoRetiredRole(t *testing.T) {
	t.Parallel()

	help := map[string]string{"Short": slingCmd.Short, "Long": slingCmd.Long}
	slingCmd.Flags().VisitAll(func(f *pflag.Flag) {
		help["--"+f.Name] = f.Usage
	})

	for _, retired := range []string{"witness", "refinery"} {
		for name, text := range help {
			if strings.Contains(strings.ToLower(text), retired) {
				t.Errorf("gt sling %s help names the retired role %q", name, retired)
			}
		}
	}
}
