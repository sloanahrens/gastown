-- Golden DDL for the cloud ledger tables the Postgres source reads. The
-- queries in postgres.go are pinned to these tables and columns by
-- TestCloudLedgerQueriesMatchTheGoldenSchema.
--
-- Source: devops internal/ledgerdb/migrations/0001_ledger_patrol_report.sql,
-- 0002_ledger_patrol_heartbeat.sql and 0003_ledger_release.sql, as they land
-- in devops commit 09bcaa41d4720129c4c89779f924c7824fa77dcb (dv-8dw9). Those
-- three files are unchanged on devops main. The statements below are copied
-- from them, not imported; the devops module is not a dependency of this one.
-- The test reads table and column names only, so a column whose type differs
-- from the migration is not caught by it.

-- One patrol run's report: the fields of pkg/patrol's Report that the town's
-- readers need (report.json's projects and findings), keyed by the run's own
-- run_id so a re-delivery of the same run is a no-op.
CREATE TABLE IF NOT EXISTS ledger_patrol_report (
    run_id         text PRIMARY KEY,
    schema_version integer NOT NULL,
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz NOT NULL,
    projects       jsonb NOT NULL,
    findings       jsonb NOT NULL
);

-- The patrol's heartbeat: when a named source last completed a run. The
-- (source, at) key is what makes a repeated heartbeat a no-op.
CREATE TABLE IF NOT EXISTS ledger_patrol_heartbeat (
    source text NOT NULL,
    at     timestamptz NOT NULL,
    PRIMARY KEY (source, at)
);

-- One releasebot ledger line. The columns mirror cmd/releasebot's ledgerLine
-- JSON fields; line holds the whole line as written, so a reader sees exactly
-- what the bot emitted. (app, at, action) is the ledger's own key, and it makes
-- a repeated append a no-op.
CREATE TABLE IF NOT EXISTS ledger_release (
    app     text NOT NULL,
    at      timestamptz NOT NULL,
    action  text NOT NULL,
    reason  text NOT NULL,
    commit  text NOT NULL,
    version text NOT NULL,
    line    bytea NOT NULL,
    CONSTRAINT ledger_release_app_at_action_key UNIQUE (app, at, action)
);
