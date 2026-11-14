-- 0004_tickets — Ticket status vocabulary (T4, docs/designs/0001 §3).
--
-- T1 left tickets.status unconstrained with the vocabulary deferred to the
-- ticket-creation ticket. The Engine is the only writer of ticket state
-- (docs/designs/0001 §6), and this is the full status vocabulary it may
-- write:
--   pending         created from an accept decision, not yet dispatched
--   running         an Assignment holds an executing A2A task
--   needs-attention waiting for a person (unresolved failure, blocked)
--   failed          a lifecycle failure transitioned the ticket out of play
--                   (T5: materialization failure)
--   ended           terminal (docs/designs/0001 §4.1: thread follows done)
ALTER TABLE tickets ADD CONSTRAINT tickets_status_check
    CHECK (status IN ('pending', 'running', 'needs-attention', 'failed', 'ended'));

-- The Outbox deliverer scans for committed-but-undelivered effects; ordered
-- by id so delivery follows enqueue order.
CREATE INDEX outbox_pending_idx ON outbox (state, id);
