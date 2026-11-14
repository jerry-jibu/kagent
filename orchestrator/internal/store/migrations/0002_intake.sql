-- 0002_intake — Source/intake state (T2, docs/designs/0001 §2 and §6).
--
-- sources   per-Source read position (cursor); the Source instance row is
--           created by the first ingested batch and the cursor only moves
--           forward (GREATEST). The 5s poll rewind re-reads a small window
--           that trigger dedup absorbs.
-- triggers.thread_key durable Thread attribution written by Intake, so a
--           trigger always knows the conversation it belongs to.
-- threads.last_activity_at sightline bookkeeping: refreshed only when a
--           trigger is actually ingested (dedup replay and sightline drops
--           never refresh it), so a thread without a live ticket expires
--           14 days after its last ingested activity (docs/designs/0001:
--           无活 Ticket 的 Thread 保留 14 天视线).

CREATE TABLE sources (
    name       TEXT        PRIMARY KEY,
    kind       TEXT        NOT NULL,
    cursor     TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE triggers ADD COLUMN thread_key TEXT NOT NULL;

ALTER TABLE threads ADD COLUMN last_activity_at TIMESTAMPTZ NOT NULL DEFAULT now();
