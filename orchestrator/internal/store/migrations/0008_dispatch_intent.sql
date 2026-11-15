-- 0008_dispatch_intent: orphan task observations for dispatch restart
-- recovery (PR #11 C3).
--
-- A dispatch that landed a task at the gateway but whose binding write was
-- lost (process crash between transport send and MarkAssignmentDispatched)
-- leaves an attempted-but-unbound assignment. Re-sending blindly would fork
-- a second worker onto the same ticket workspace. Observations of such tasks
-- are persisted here; the dispatcher's restart recovery adopts (binds and
-- applies) the recorded task instead of re-sending.
--
-- Key: the assignment id when the unbound send recorded the observation
-- (the authoritative case); the gateway context id when the push receiver
-- observed a task no assignment is bound to (adoption by session id where
-- one exists). One row per key -- the latest observation wins; adoption
-- claims and deletes the row in one transaction, so exactly one process can
-- adopt a given observation.
CREATE TABLE IF NOT EXISTS orphan_task_observations (
    observation_key TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL,
    state           TEXT NOT NULL,
    status_text     TEXT NOT NULL DEFAULT '',
    observed_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
