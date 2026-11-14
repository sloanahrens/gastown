package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// runReady is the container's health check: the same three questions GET
// /readyz asks, answered by a short-lived process with no server and no token.
func runReady(args []string, stderr io.Writer) int {
	cf := defaultConfig()
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bindShared(fs, &cf)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bdgw ready: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := cf.require("tokens-dir", "verbs", "workspace"); err != nil {
		fmt.Fprintln(stderr, "bdgw ready:", err)
		return exitUsage
	}
	if err := checkReady(cf.tokensDir, cf.verbsPath, cf.workspaceRoot); err != nil {
		fmt.Fprintln(stderr, "bdgw ready: not ready:", err)
		return exitFailed
	}
	return exitOK
}

// checkReady loads what the gateway loads before it serves: the token
// directory, the verbs file and a workspace tree. The workspace is read, never
// recovered or swapped, so the check is safe while the gateway is pushing. bd
// itself is not started: a health check that spawned a process could itself be
// the load that fails it, and a missing binary is a first-call failure.
func checkReady(tokensDir, verbsPath, workspaceRoot string) error {
	if _, err := bdgateway.NewAuthenticator(tokensDir); err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	if _, err := bdgateway.LoadPolicyFile(verbsPath); err != nil {
		return fmt.Errorf("verbs: %w", err)
	}
	present, err := bdgateway.WorkspacePresent(workspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !present {
		return errors.New("workspace: no files are loaded")
	}
	return nil
}
