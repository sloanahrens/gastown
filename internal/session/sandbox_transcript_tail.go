package session

import (
	"context"
	"fmt"
	"strings"
)

// transcriptTail reads the last 16 KiB of the transcript at path in session's
// sandbox, refusing any path that is not a plain transcript path.
func (b *openshellBackend) transcriptTail(ctx context.Context, session, path string) ([]byte, error) {
	rel, ok := strings.CutPrefix(path, sandboxProjectsDir+"/")
	if !ok || !plainTranscriptPath(rel) {
		return nil, fmt.Errorf("sandbox transcript tail: path is not a transcript under %s", sandboxProjectsDir)
	}
	ctx, cancel := context.WithTimeout(ctx, transcriptTimeout)
	defer cancel()
	out, _, err := b.cli.Exec(ctx, session, nil, []string{"tail", "-c", "16384", "--", path}, nil)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: reading transcript tail: %w", session, err)
	}
	return out, nil
}
