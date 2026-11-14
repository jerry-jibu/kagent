-- 0005_dispatch — dispatch-chain state (T6, docs/designs/0001 D2 + §4.3).
--
-- assignments.report      last report state the assignment settled under
--                         (progress/waiting/ready/blocked/failed/dropped);
--                         the Pass follow-up scan keys off it.
-- assignments.dispatch_attempts / last_error   the dispatch ledger: how many
--                         SendMessages failed before one landed; a retry past
--                         the first failure carries the recovery annotation.
-- assignments.kind        initial (created with the ticket) vs followup
--                         (continuation after a progress report); the cap
--                         counts followups per ticket.
-- assignments_task_id_idx unique: one task id binds to at most one
--                         assignment — the push receiver's lookup key.
-- assignments_status_idx  the Pass scans by status every 2s beat.
-- tickets.trigger_source / trigger_deterministic_id   the originating
--                         trigger's identity, recorded at creation so the
--                         dispatch document can carry the original request.

ALTER TABLE assignments ADD COLUMN report            TEXT    NOT NULL DEFAULT '';
ALTER TABLE assignments ADD COLUMN dispatch_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assignments ADD COLUMN last_error        TEXT    NOT NULL DEFAULT '';
ALTER TABLE assignments ADD COLUMN kind              TEXT    NOT NULL DEFAULT 'initial'
    CHECK (kind IN ('initial', 'followup'));

CREATE UNIQUE INDEX assignments_task_id_idx ON assignments (task_id) WHERE task_id IS NOT NULL;
CREATE INDEX assignments_status_idx ON assignments (status);

ALTER TABLE tickets ADD COLUMN trigger_source            TEXT NOT NULL DEFAULT '';
ALTER TABLE tickets ADD COLUMN trigger_deterministic_id  TEXT NOT NULL DEFAULT '';
