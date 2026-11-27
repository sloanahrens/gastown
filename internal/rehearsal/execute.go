package rehearsal

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Execute runs one rehearsal with Drive. On success it prints the verdict to stdout with
// Finish and returns Finish's code. On a Drive error it prints one stderr line starting
// "gt-rehearsal: run: " with newlines replaced by "; ", prints no verdict and returns 1.
func Execute(ctx context.Context, o Options, stdout, stderr io.Writer) int {
	rec, err := Drive(ctx, o)
	if err != nil {
		fmt.Fprintln(stderr, "gt-rehearsal: run: "+strings.ReplaceAll(err.Error(), "\n", "; "))
		return 1
	}
	return Finish(rec, stdout)
}
