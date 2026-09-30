package config

import (
	"errors"
	"os"
	"strings"
)

// fakeHost is a host whose environment is env, whose PATH holds exactly bins
// (each found at /fake/bin/<name>, which lookPath also accepts as a path),
// and which has no working directory; see inDir. Tests resolve agents against it instead of setting env, writing PATH
// stubs or chdir-ing.
func fakeHost(env map[string]string, bins ...string) host {
	onPath := make(map[string]bool, len(bins))
	for _, b := range bins {
		onPath[b] = true
	}
	return host{
		getenv: func(key string) string { return env[key] },
		lookPath: func(name string) (string, error) {
			if onPath[name] || onPath[strings.TrimPrefix(name, "/fake/bin/")] {
				return "/fake/bin/" + strings.TrimPrefix(name, "/fake/bin/"), nil
			}
			return "", &os.PathError{Op: "lookpath", Path: name, Err: os.ErrNotExist}
		},
		getwd: func() (string, error) { return "", errors.New("fakeHost has no working directory") },
	}
}

// inDir returns h with dir as its working directory.
func (h host) inDir(dir string) host {
	h.getwd = func() (string, error) { return dir, nil }
	return h
}

// agentBins are the agent binaries the old TestMain stubbed onto PATH.
var agentBins = []string{"claude", "gemini", "codex", "cursor-agent", "auggie", "amp", "opencode"}

// agentHost is fakeHost with every agent in agentBins on PATH, plus extra.
func agentHost(env map[string]string, extra ...string) host {
	return fakeHost(env, append(append([]string(nil), agentBins...), extra...)...)
}
