package git

import (
	"strconv"
	"strings"
	"time"
)

// InitRepo makes the working directory a repository with branch as its
// initial branch (git init -b). Run on an existing repository, git leaves it
// as it is.
func (g *Git) InitRepo(branch string) error {
	_, err := g.run("init", "-b", branch)
	return err
}

// ConfigSet sets a repository config value (git config <key> <value>).
func (g *Git) ConfigSet(key, value string) error {
	_, err := g.run("config", key, value)
	return err
}

// CommitWithAuthor commits the index with author as the commit's author
// ("Name <email>").
func (g *Git) CommitWithAuthor(message, author string) error {
	_, err := g.run("commit", "-m", message, "--author="+author)
	return err
}

// PackSize is the size-pack line of git count-objects -v: the KiB the
// repository's packs take on disk, as git printed it, or "" when git
// printed none.
func (g *Git) PackSize() (string, error) {
	out, err := g.run("count-objects", "-v")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if kb, ok := strings.CutPrefix(line, "size-pack:"); ok {
			return strings.TrimSpace(kb), nil
		}
	}
	return "", nil
}

// LogEntry is one commit of LogAll: its id, committer time (zero when git's
// date did not parse) and subject line.
type LogEntry struct {
	Hash    string
	Time    time.Time
	Subject string
}

// logFieldSep separates LogAll's format fields; a subject never holds it.
const logFieldSep = "\x1f"

// LogAll lists up to max commits reachable from any ref, newest first by
// ancestry (git log --all --topo-order). A repository with no commits lists
// none.
func (g *Git) LogAll(max int) ([]LogEntry, error) {
	out, err := g.run("log", "--all", "--topo-order", "--max-count", strconv.Itoa(max),
		"--format=%H"+logFieldSep+"%cI"+logFieldSep+"%s")
	if err != nil {
		return nil, err
	}
	var entries []LogEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, logFieldSep, 3)
		if len(fields) != 3 {
			continue
		}
		e := LogEntry{Hash: fields[0], Subject: fields[2]}
		if t, err := time.Parse(time.RFC3339, fields[1]); err == nil {
			e.Time = t
		}
		entries = append(entries, e)
	}
	return entries, nil
}
