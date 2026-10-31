-- External dependents index: the reverse read of a target named as
-- external:<rig>:<id> keeps the external links whose last part is that id. This
-- partial expression index holds only the external links, keyed on that last
-- part, so the read is an index lookup. The plain links keep dependencies_target_idx.
-- The predicate and the expression here are the ones fillDependentsSQL uses.
-- No table changes.
CREATE INDEX dependencies_external_target_idx
    ON tracker.dependencies ((regexp_replace(depends_on, '^.*:', '')))
    WHERE depends_on LIKE 'external:%:%';
