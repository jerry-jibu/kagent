-- 0010_hotpath_delivery — hot-path indexes + GitHub rate-limit delivery
-- deferral (PR #11 review, Required #3).
--
-- assignments(ticket_id, created_at DESC, id DESC)
--     the latest-assignment CTE (predicates.go: waitsForPerson/followupOwners/
--     admin list) runs DISTINCT ON (ticket_id) ... ORDER BY ticket_id,
--     created_at DESC, id DESC on every scan, and Assignments(ticket) orders
--     by (created_at, id) — the same index read backwards. Before this index
--     both sorted every assignment row per ticket.
-- triggers(source, thread_key, created_at)
--     the waitsForPerson "nobody has spoken since" probe (NOT EXISTS ...
--     WHERE source = tk.thread_source AND thread_key = tk.thread_key AND
--     created_at > settled_at) matched only on the (source, deterministic_id)
--     primary key prefix before — one probe per live ticket per Pass beat.
-- tickets(thread_source, thread_key)
--     FollowupOwnerOfThread (followupOwners: the newest live ticket of a
--     thread) and the ingest sightline's live-ticket EXISTS probe filtered by
--     thread identity via a full scan before.
-- tickets.rate_limited_until
--     the delivery-side GitHub rate-limit deferral: DeferTicketDelivery sets
--     it (CAS-guarded on delivery_state = pending), PendingDeliveries hides
--     deferred tickets until the recorded Retry-After instant. NULL = never
--     deferred. The audit observation is the deliverer's own delivery.failed
--     row (rate_limited/retry_after detail) — the store writes no audit here.

CREATE INDEX assignments_ticket_created_idx
    ON assignments (ticket_id, created_at DESC, id DESC);

CREATE INDEX triggers_thread_idx
    ON triggers (source, thread_key, created_at);

CREATE INDEX tickets_thread_idx
    ON tickets (thread_source, thread_key);

ALTER TABLE tickets ADD COLUMN rate_limited_until TIMESTAMPTZ;
