package cmd

import (
	"context"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/sbxcreds"
)

// runClusterCredentials refuses a bad request through credentialRequest before run is reached, then
// issues the kubeconfig to outPath and prints formatCredential's line for role through sbxcreds.IssueLine.
func runClusterCredentials(ctx context.Context, run func(ctx context.Context, argv []string) ([]byte, error), out io.Writer, cluster, role, outPath string) error {
	req, err := credentialRequest(cluster, role, outPath)
	if err != nil {
		return err
	}
	format := func(subject string, notAfter time.Time) (string, error) {
		return formatCredential(role, subject, notAfter)
	}
	return sbxcreds.IssueLine(ctx, run, req, outPath, format, out)
}
