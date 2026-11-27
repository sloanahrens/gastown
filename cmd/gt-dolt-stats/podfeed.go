package main

import (
	"errors"
	"path/filepath"
	"strings"
)

// podAppendArgv builds the kubectl exec argv that appends one line to path on
// stdin inside the daemon pod: the same command gt-mac-watch publishes with
// (docs/design/pod-host-probes.md) (gt-deyva8).
//
// The path is single-quoted for sh, which makes $ and backticks literal, so it
// is refused when it holds a single quote or a newline; a relative path would
// depend on the container's working directory and is refused too.
func podAppendArgv(context, namespace, path string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("pod append path is not absolute")
	}
	if strings.ContainsRune(path, '\'') {
		return nil, errors.New("pod append path holds a single quote")
	}
	if strings.ContainsRune(path, '\n') {
		return nil, errors.New("pod append path holds a newline")
	}
	return []string{
		"kubectl", "--context", context, "-n", namespace, "exec", "-i",
		"deploy/daemon", "-c", "daemon", "--", "sh", "-c", "cat >> '" + path + "'",
	}, nil
}
