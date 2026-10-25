-- A token has a scope. A seat token can use every issue verb; an admin token
-- is also needed for configuration writes, import and export, token issuance
-- and migrations, and is held by the daemon and operators, never by a seat.
-- Existing tokens become seat tokens.
ALTER TABLE tracker.tokens
    ADD COLUMN scope text NOT NULL DEFAULT 'seat' CHECK (scope IN ('seat', 'admin'));
