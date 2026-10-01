package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testdb"
)

// The bd init template (gt-ik4a1.4).
//
// A bd init on an empty database runs every schema migration, one
// DOLT_COMMIT each, before it records the workspace's identity: about 1.5s of
// its 2.5s on an idle container, and most of what a container-backed package
// spends in bd init. So the pool's bd init databases (doltDBPool.inits) start
// as clones of a database bd init migrated once, taken at the commit before
// bd init recorded its identity. A bd init on one finds nothing to migrate
// and only records the identity. A lease ends by resetting the database to
// that commit, so every lessee gets an identity-free, migrated database.
//
// The template is made by the bd on PATH, the one the tests' own inits run,
// so its schema is the one they expect; a newer bd migrates the rest itself.

// doltPoolInits is how many template databases the pool holds for bd inits.
// It bounds how many bd init leases get one at once, not how many a run
// uses: a lease beyond it gets an empty store database and migrates it, as
// every bd init did before the template.
const doltPoolInits = 24

// doltInitTemplateDB is the database bd init migrates for the template. It
// is dropped before any test can reach the container.
const doltInitTemplateDB = testdb.MintPrefix + "bdinit_template"

// doltInitTemplateRemote is where the template commit is pushed for the
// clones: a file remote inside the container, gone with it.
const doltInitTemplateRemote = "file:///tmp/gt-pool-bdinit-template"

// doltInitTemplateBranch names the template commit for the push.
const doltInitTemplateBranch = "gt_bdinit_template"

// addInits lays out n template databases for bd init leases, without touching
// the server.
func (p *doltDBPool) addInits(n int) {
	for i := range n {
		e := &doltPoolEntry{name: fmt.Sprintf("%sbdinit_%04d", testdb.MintPrefix, i)}
		p.inits = append(p.inits, e)
		p.names[e.name] = true
	}
}

// createInits makes the template and clones every bd init database from it,
// recording each one's initial commit. Like create, it must run before any
// test can reach the server.
func (p *doltDBPool) createInits(ctx context.Context) error {
	if len(p.inits) == 0 {
		return nil
	}
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer releaseResetConn(conn)
	exec := func(q string, args ...any) error {
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
		return nil
	}
	if err := p.makeInitTemplate(ctx, conn, exec); err != nil {
		return fmt.Errorf("bd init template: %w", err)
	}
	for i, e := range p.inits {
		err := exec("CALL DOLT_CLONE(?, ?)", doltInitTemplateRemote, e.name)
		if err == nil {
			err = exec("USE `" + e.name + "`")
		}
		if err == nil {
			err = exec("CALL DOLT_REMOTE('remove', 'origin')")
		}
		if err == nil {
			e.initCommit, err = doltHead(ctx, p.db, e.name)
			e.head = e.initCommit
		}
		if err != nil {
			return fmt.Errorf("clone bd init database %d of %d (%s): %w", i+1, len(p.inits), e.name, err)
		}
	}
	return nil
}

// makeInitTemplate runs bd init on a database of its own, pushes the commit
// before bd init's identity commit to doltInitTemplateRemote, and drops the
// database.
func (p *doltDBPool) makeInitTemplate(ctx context.Context, conn *sql.Conn, exec func(string, ...any) error) error {
	if err := exec("CREATE DATABASE `" + doltInitTemplateDB + "`"); err != nil {
		return err
	}
	dir := filepath.Join(p.base, "bdinit-template")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	args := []string{"init", "--quiet", "--prefix", "tmpl", "--server", "--server-port", strconv.Itoa(p.port), "--database", doltInitTemplateDB}
	// bd refuses a test database on a server not declared a test server.
	env := beads.StripEnvKey(os.Environ(), "BEADS_TEST_SERVER")
	env = append(env, "BEADS_TEST_SERVER=1")
	if out, err := beads.RunTestContainerInit(ctx, dir, args, env); err != nil {
		return fmt.Errorf("bd init: %w\n%s", err, out)
	}
	if err := exec("USE `" + doltInitTemplateDB + "`"); err != nil {
		return err
	}
	var head, migrated, message string
	if err := conn.QueryRowContext(ctx, "SELECT DOLT_HASHOF('main'), DOLT_HASHOF('main~1')").Scan(&head, &migrated); err != nil {
		return fmt.Errorf("read bd init's commits: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT message FROM dolt_log WHERE commit_hash = ?", head).Scan(&message); err != nil {
		return fmt.Errorf("read bd init's last commit: %w", err)
	}
	if message != "bd init" {
		return fmt.Errorf("bd init's last commit is %q, want \"bd init\": the template would not be the commit before its identity", message)
	}
	// The template must hold no identity, or every lessee would share it.
	var identity int
	q := "SELECT (SELECT COUNT(*) FROM config AS OF '" + migrated + "' WHERE `key` = 'issue_prefix')" +
		" + (SELECT COUNT(*) FROM metadata AS OF '" + migrated + "' WHERE `key` = '_project_id')"
	if err := conn.QueryRowContext(ctx, q).Scan(&identity); err != nil {
		return fmt.Errorf("check the template holds no identity: %w", err)
	}
	if identity != 0 {
		return fmt.Errorf("the commit before bd init's (%s) already holds a prefix or project id", migrated)
	}
	for _, step := range []struct {
		q    string
		args []any
	}{
		{"CALL DOLT_BRANCH(?, ?)", []any{doltInitTemplateBranch, migrated}},
		{"CALL DOLT_REMOTE('add', ?, ?)", []any{doltInitTemplateBranch, doltInitTemplateRemote}},
		{"CALL DOLT_PUSH(?, ?)", []any{doltInitTemplateBranch, doltInitTemplateBranch + ":main"}},
		{"USE information_schema", nil},
		{"DROP DATABASE `" + doltInitTemplateDB + "`", nil},
		{"CALL dolt_purge_dropped_databases()", nil},
	} {
		if err := exec(step.q, step.args...); err != nil {
			return err
		}
	}
	return nil
}
