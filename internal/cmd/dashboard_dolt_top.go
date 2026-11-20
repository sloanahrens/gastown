package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The Dolt panel's top section: the connections the town's server lists and the
// statement shapes keeping it busiest. It reads the process list the way
// `gt dolt top` reads it and ranks with the command's own function, so the page
// and the command cannot disagree about which shapes are busiest (gt-jurq4x).

const (
	// doltTopSectionReads is how many process-list reads the section takes. Two
	// is the fewest that tells one shape running now from one still running a
	// second later.
	doltTopSectionReads = 2
	// doltTopSectionInterval is the gap between them: one second, so the later
	// read sees a new moment rather than the earlier read's statements again.
	doltTopSectionInterval = time.Second
	// doltTopSectionLimit is how many shapes the section draws.
	doltTopSectionLimit = 2
	// doltTopSectionTitle is the section's heading on the page.
	doltTopSectionTitle = "Dolt: connections and top statements"
)

// doltTopSectionDeps is the section's contact with the outside world: the
// reader the samples come from and the clock the wait between its reads runs
// on. A test supplies a fake reader and a sleep that records rather than waits.
type doltTopSectionDeps struct {
	open  func(ctx context.Context) (doltTopReader, error)
	sleep func(ctx context.Context, d time.Duration) error
}

// doltTopSectionRealDeps is doltTopSectionDeps on the real machine.
func doltTopSectionRealDeps() doltTopSectionDeps {
	return doltTopSectionDeps{open: openDoltTopSection, sleep: sleepDoltTop}
}

func init() {
	registerDoltSection("top", func(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
		return readDoltTopSection(ctx, doltTopSectionRealDeps())
	})
}

// readDoltTopSection takes the section's reads and draws it. A read that does
// not complete is an error, never a section drawn from fewer reads: a share of
// "1 of 2" would name a read that never happened.
func readDoltTopSection(ctx context.Context, deps doltTopSectionDeps) (dashboard.DoltSection, error) {
	reader, err := deps.open(ctx)
	if err != nil {
		return dashboard.DoltSection{}, err
	}
	// The section is drawn before the connection closes, so a close error
	// cannot change it.
	defer func() { _ = reader.Close() }()

	readings := make([][]doltTopSample, 0, doltTopSectionReads)
	for i := 0; i < doltTopSectionReads; i++ {
		if i > 0 {
			if err := deps.sleep(ctx, doltTopSectionInterval); err != nil {
				return dashboard.DoltSection{}, err
			}
		}
		got, err := reader.ReadProcessList(ctx)
		if err != nil {
			return dashboard.DoltSection{}, fmt.Errorf("reading the process list: %w", err)
		}
		readings = append(readings, got)
	}
	return doltTopSection(readings[0], readings[1]), nil
}

// openDoltTopSection connects the section to the town's Dolt server the way
// `gt dolt top` connects. The town root comes from the panel's context rather
// than from the process's directory, and it must be a directory: DefaultConfig
// answers a missing town with the default address, so dialing a root that is
// not there would report on a server the panel was not asked about. A dial
// error names the address and never the credential.
func openDoltTopSection(ctx context.Context) (doltTopReader, error) {
	root, ok := doltTownRoot(ctx)
	if !ok {
		return nil, errors.New("the Dolt panel gave the top section no town root")
	}
	opener := doltTopOpener{
		findTownRoot: func() (string, error) {
			info, err := os.Stat(root)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", root)
			}
			return root, nil
		},
		openDB: func(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) },
	}
	return opener.open(ctx)
}

// doltTopSection draws the section from its two reads: how many connections the
// later read listed, and the two busiest statement shapes by the share of the
// reads that saw each running. It ranks with doltTopRanking, the function
// `gt dolt top` prints from, so one shape cannot be busiest on the page and
// second in the table.
func doltTopSection(first, second []doltTopSample) dashboard.DoltSection {
	readings := [][]doltTopSample{first, second}
	section := dashboard.DoltSection{
		Title: doltTopSectionTitle,
		Note:  doltTopConnectionCount(len(second)),
		Head:  []string{"SHARE", "STATEMENT"},
	}
	for _, row := range doltTopRanking(readings, doltTopSectionLimit) {
		section.Rows = append(section.Rows, []string{
			fmt.Sprintf("%d%%", 100*row.Samples/len(readings)),
			row.Statement,
		})
	}
	return section
}

// doltTopConnectionCount is the section's note: the server's connection count
// in words, so the page reads "1 connection" rather than "1 connections".
func doltTopConnectionCount(n int) string {
	if n == 1 {
		return "1 connection"
	}
	return fmt.Sprintf("%d connections", n)
}
