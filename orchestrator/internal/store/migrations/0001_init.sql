-- 0001_init — orchestrator schema baseline (T1).
--
-- Tables follow docs/designs/0001 §3 object model and §6 transaction rules:
--   triggers   immutable events, deduplicated by (source, deterministic_id)
--   threads    external conversation keys (source, thread_key)
--   tickets    authorized work units; mandate carries the policy version hash
--   assignments ticket↔A2A task bindings
--   outbox     transactional effects; (channel, idempotency_key) unique for receipts
--   policy     applied policy version hashes (audit + mandate reference)
--   timers     RESERVED (docs/designs/0004 G1): follow-up + schedule timers,
--              fields aligned to the Anthropic structure; logic lands in P1.

CREATE TABLE triggers (
    source           TEXT        NOT NULL,
    deterministic_id TEXT        NOT NULL,
    payload          JSONB       NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, deterministic_id)
);

CREATE TABLE threads (
    source     TEXT        NOT NULL,
    thread_key TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, thread_key)
);

CREATE TABLE tickets (
    id             TEXT        PRIMARY KEY,
    thread_source  TEXT        NOT NULL,
    thread_key     TEXT        NOT NULL,
    mandate        JSONB       NOT NULL, -- policy version hash + sender + scope + strength
    status         TEXT        NOT NULL,
    session_id     TEXT,                 -- kagent session (continuity)
    workspace_name TEXT,                 -- agentfs workspace (per-ticket dir)
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE assignments (
    id         TEXT        PRIMARY KEY,
    ticket_id  TEXT        NOT NULL REFERENCES tickets (id),
    task_id    TEXT,                   -- A2A task id, set once dispatched
    status     TEXT        NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'running', 'ended', 'unresolved')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE outbox (
    id              BIGSERIAL   PRIMARY KEY,
    channel         TEXT        NOT NULL,
    address         TEXT        NOT NULL,
    idempotency_key TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    receipt         JSONB,                -- channel receipt (deterministic marker / message id)
    state           TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'delivered')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    UNIQUE (channel, idempotency_key)
);

CREATE TABLE policy (
    version    TEXT        PRIMARY KEY, -- SHA256 hash of the canonical policy content
    content    JSONB       NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE timers (
    id         TEXT        PRIMARY KEY,
    kind       TEXT        NOT NULL CHECK (kind IN ('followup', 'schedule')),
    ticket_id  TEXT,                    -- nullable: schedule timers may be thread-scoped
    fire_at    TIMESTAMPTZ NOT NULL,
    period     TEXT,                    -- repetition spec, schedule kind only
    payload    JSONB       NOT NULL,
    state      TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'fired')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- schema_migrations is owned by the migration runner and created before any
-- migration file is applied.
