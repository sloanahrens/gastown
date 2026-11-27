package sbxcreds

import (
	"context"
	"fmt"
	"io"
	"time"
)

// IssueLine issues the request with Issue, then prints format's line for the certificate to out
// followed by a newline. An Issue or format error is returned and nothing is printed. Issue writes
// the kubeconfig before format runs, so a format error leaves it at path.
func IssueLine(ctx context.Context, run func(ctx context.Context, argv []string) ([]byte, error), req Request, path string, format func(subject string, notAfter time.Time) (string, error), out io.Writer) error {
	subject, notAfter, err := Issue(ctx, run, req, path)
	if err != nil {
		return err
	}
	line, err := format(subject, notAfter)
	if err != nil {
		return fmt.Errorf("the line is refused: %w", err)
	}
	if _, err := fmt.Fprintln(out, line); err != nil {
		return fmt.Errorf("could not print the line: %w", err)
	}
	return nil
}
