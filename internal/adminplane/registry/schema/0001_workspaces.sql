-- The admin plane's workspace registry (plan row V1b): one row per hosted
-- workspace, the org and owner it belongs to, and the state the registry
-- package moves it through. The state check constraint is this table's copy of
-- the package's four State constants, and TestStateCheckConstraintListsTheFourStates
-- holds the two together.
CREATE SCHEMA IF NOT EXISTS adminplane;

CREATE TABLE adminplane.workspaces (
    id         text PRIMARY KEY,
    org_id     text NOT NULL,
    owner_sub  text NOT NULL,
    state      text NOT NULL CHECK (state IN ('active', 'paused', 'suspended', 'closed')),
    created_at timestamptz NOT NULL DEFAULT now()
);
