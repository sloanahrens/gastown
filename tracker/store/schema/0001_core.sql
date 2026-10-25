-- Core tracker schema (the tracker design, section 4). One schema, one issues table for issues and wisps, one
-- dependencies table, one append-only events table. The mail and receipt
-- tables are a later migration.

CREATE TABLE tracker.rigs (
    prefix     text PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tracker.issues (
    id                  text PRIMARY KEY,
    rig                 text NOT NULL REFERENCES tracker.rigs (prefix),
    issue_type          text NOT NULL,
    status              text NOT NULL,
    title               text NOT NULL,
    description         text NOT NULL DEFAULT '',
    design              text NOT NULL DEFAULT '',
    acceptance_criteria text NOT NULL DEFAULT '',
    notes               text NOT NULL DEFAULT '',
    priority            smallint NOT NULL DEFAULT 2,
    assignee            text NOT NULL DEFAULT '',
    owner               text NOT NULL DEFAULT '',
    created_by          text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    started_at          timestamptz,
    closed_at           timestamptz,
    defer_until         timestamptz,
    close_reason        text NOT NULL DEFAULT '',
    ephemeral           boolean NOT NULL DEFAULT false,
    wisp_type           text NOT NULL DEFAULT '',
    event_kind          text NOT NULL DEFAULT '',
    payload             jsonb,
    -- Agent beads (issue_type agent) carry their slots here.
    hook_bead           text NOT NULL DEFAULT '',
    agent_state         text NOT NULL DEFAULT '',
    last_activity       timestamptz,
    -- Kept equal to the parent-child dependency by the code that writes one.
    parent_id           text,
    -- Everything else a legacy row carries; the import stores unmapped
    -- columns here instead of dropping them.
    metadata            jsonb NOT NULL DEFAULT '{}',
    CONSTRAINT issues_id_has_rig_prefix CHECK (starts_with(id, rig || '-'))
);

-- Hot paths: the board (not closed, by rig), a seat's work (by assignee),
-- children (by parent), the wisp reaper (ephemeral, by status and age).
CREATE INDEX issues_active_idx    ON tracker.issues (rig, status)        WHERE status <> 'closed';
CREATE INDEX issues_assignee_idx  ON tracker.issues (assignee, status)   WHERE assignee <> '';
CREATE INDEX issues_parent_idx    ON tracker.issues (parent_id)          WHERE parent_id IS NOT NULL;
CREATE INDEX issues_ephemeral_idx ON tracker.issues (status, closed_at)  WHERE ephemeral;
CREATE INDEX issues_type_idx      ON tracker.issues (issue_type, status);
CREATE INDEX issues_updated_idx   ON tracker.issues (updated_at);

-- One table for issues and wisps. depends_on is an issue or wisp id, or an
-- external:<rig>:<id> reference to something outside this database, so it
-- has no foreign key. Deleting an issue removes its outgoing links; the code
-- that deletes also removes the incoming ones in the same transaction.
CREATE TABLE tracker.dependencies (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id   text NOT NULL REFERENCES tracker.issues (id) ON DELETE CASCADE,
    depends_on text NOT NULL,
    type       text NOT NULL,
    metadata   jsonb,
    thread_id  text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text NOT NULL DEFAULT '',
    UNIQUE (issue_id, depends_on)
);
CREATE INDEX dependencies_target_idx      ON tracker.dependencies (depends_on);
CREATE INDEX dependencies_type_target_idx ON tracker.dependencies (type, depends_on);
CREATE INDEX dependencies_type_issue_idx  ON tracker.dependencies (type, issue_id);

CREATE TABLE tracker.labels (
    issue_id text NOT NULL REFERENCES tracker.issues (id) ON DELETE CASCADE,
    label    text NOT NULL,
    PRIMARY KEY (issue_id, label)
);
CREATE INDEX labels_label_idx ON tracker.labels (label, issue_id);

CREATE TABLE tracker.comments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    issue_id   text NOT NULL REFERENCES tracker.issues (id) ON DELETE CASCADE,
    author     text NOT NULL,
    text       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX comments_issue_idx ON tracker.comments (issue_id, created_at);

-- Append-only audit and replay feed. issue_id has no foreign key on purpose:
-- an issue's events outlive the issue (beads' ON DELETE CASCADE lost them).
-- Retention may null payload; it never deletes a row.
CREATE TABLE tracker.events (
    seq       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts        timestamptz NOT NULL DEFAULT now(),
    op        text NOT NULL,
    issue_id  text NOT NULL,
    actor     text NOT NULL DEFAULT '',
    old_value text,
    new_value text,
    comment   text,
    payload   jsonb
);
CREATE INDEX events_issue_idx ON tracker.events (issue_id, seq);
CREATE INDEX events_ts_idx    ON tracker.events (ts);

CREATE TABLE tracker.kv (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tracker.config (
    key   text PRIMARY KEY,
    value text NOT NULL
);

CREATE TABLE tracker.custom_types (
    name text PRIMARY KEY
);

CREATE TABLE tracker.custom_statuses (
    name     text PRIMARY KEY,
    category text NOT NULL DEFAULT 'unspecified'
);

CREATE TABLE tracker.child_counters (
    parent_id  text PRIMARY KEY,
    last_child integer NOT NULL DEFAULT 0
);

-- Bearer tokens, stored hashed; the actor comes from the token.
CREATE TABLE tracker.tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor      text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz,
    revoked_at timestamptz
);
