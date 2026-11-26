package sbxcreds

import (
	"context"
	"fmt"
	"time"
)

// Issue runs the request's kubeadm argv through run, refuses output that does not carry a client
// certificate, writes the printed kubeconfig to path and returns the certificate's subject and
// not-after. Every refusal returns zero values, and a refusal before the write leaves path
// untouched; a write error is returned as writeKubeconfig reports it. The output holds the key,
// so the runner's error is not wrapped: its text can quote the output.
func Issue(ctx context.Context, run func(ctx context.Context, argv []string) ([]byte, error), req Request, path string) (subject string, notAfter time.Time, err error) {
	argv, err := req.Argv()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the request is refused: %w", err)
	}
	out, err := run(ctx, argv)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("kubeadm printed no kubeconfig for client %q", req.ClientName)
	}
	subject, notAfter, err = certInfo(out)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("the printed kubeconfig is refused: %w", err)
	}
	if err := writeKubeconfig(path, out); err != nil {
		return "", time.Time{}, err
	}
	return subject, notAfter, nil
}
