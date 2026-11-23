package cmd

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// TestTownDeployerServeCommand: serve is registered under gt town deployer, takes no
// arguments, marks four flags required, and hands the run their values (gt-p1ej8c).
func TestTownDeployerServeCommand(t *testing.T) {
	found, _, err := townDeployerCmd.Find([]string{"serve"})
	if err != nil || found != townDeployerServeCmd || found.Use != "serve" {
		t.Fatalf("gt town deployer Find(serve) = %v, %v; want the serve command", found, err)
	}
	if err := found.Args(found, []string{"extra"}); err == nil {
		t.Error("serve accepted an argument, want a command that takes none")
	}
	for _, name := range []string{"as-user", "inbox", "answers", "state"} {
		f := found.Flags().Lookup(name)
		if f == nil || !slices.Equal(f.Annotations[cobra.BashCompOneRequiredFlag], []string{"true"}) {
			t.Errorf("--%s = %v, want a flag marked required", name, f)
		}
	}
	// The run is handed the flags' values: the request answers and --state/done takes it.
	inbox, answers, state := newServeTestDirs(t)
	writeServeRequest(t, inbox, serveOlderID)
	steps := &serveFakeSteps{}
	serveSteps = steps
	var out bytes.Buffer
	found.SetOut(&out)
	t.Cleanup(func() {
		serveSteps = nil
		found.SetOut(nil)
		found.Flags().VisitAll(func(f *pflag.Flag) { _ = f.Value.Set(f.DefValue) })
	})
	if err := found.Flags().Parse([]string{"--once", "--as-user", "deployer", "--inbox", inbox, "--answers", answers, "--state", state}); err != nil {
		t.Fatalf("parsing the flags: %v", err)
	}
	if err := found.RunE(found, nil); err != nil {
		t.Fatalf("gt town deployer serve = %v", err)
	}
	if steps.calls != 1 {
		t.Errorf("the steps planned %d times, want 1: --once reached the run", steps.calls)
	}
	if got := serveNames(t, filepath.Join(state, "done")); !slices.Equal(got, []string{serveOlderID + ".json"}) {
		t.Errorf("done holds %v, want the request: --state reached the run", got)
	}
	if !strings.Contains(out.String(), "id: "+serveOlderID) {
		t.Errorf("output = %q, want the answered id on the command's output", out.String())
	}
}

// TestTownDeployerServeCommandFlagErrors: a flag read that fails is an error naming that flag.
func TestTownDeployerServeCommandFlagErrors(t *testing.T) {
	t.Parallel()
	names := []string{"once", "inbox", "answers", "state"}
	for i, missing := range names {
		t.Run("reading --"+missing, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range names[:i] {
				if name == "once" {
					cmd.Flags().Bool(name, false, "")
				} else {
					cmd.Flags().String(name, "", "")
				}
			}
			err := runTownDeployerServe(cmd, nil)
			if want := "reading --" + missing; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("runTownDeployerServe = %v, want a refusal naming %q", err, want)
			}
		})
	}
}
