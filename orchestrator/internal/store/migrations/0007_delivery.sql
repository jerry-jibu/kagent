-- 0007_delivery — result delivery state (T7, docs/designs/0003 Alternative E:
-- the orchestrator computes the patch from the workspace read-back and ships
-- branch/PR/comment; 0001 §4.1 ⑥⑦).
--
-- tickets.delivery_state   one result per ticket, four states:
--                            ''         nothing to deliver (never ready)
--                            pending    a ready report owes a PR + result
--                                       comment; the delivery scan's universe
--                            delivered  PR open + comment effect enqueued
--                                       (pending→delivered is one-way)
--                            failed     patch missing/corrupted/empty — the
--                                       ticket failed, no PR was pushed
-- tickets.base_branch / base_commit   the materialization baseline (0003 路线 A:
--                            浅克隆基线 = issue 指向分支 HEAD), recorded by the
--                            materializer; the deliverer re-clones this exact
--                            commit so the diff never includes upstream drift.

ALTER TABLE tickets ADD COLUMN delivery_state TEXT NOT NULL DEFAULT ''
    CHECK (delivery_state IN ('', 'pending', 'delivered', 'failed'));
ALTER TABLE tickets ADD COLUMN base_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE tickets ADD COLUMN base_commit TEXT NOT NULL DEFAULT '';

CREATE INDEX tickets_delivery_idx ON tickets (delivery_state) WHERE delivery_state = 'pending';
