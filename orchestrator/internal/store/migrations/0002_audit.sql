-- 0002_audit — ticket lifecycle audit trail (T5).
--
-- The audit table records orchestrator-observed lifecycle events
-- (materialization runs, failure transitions) per ticket. It is the
-- orchestrator-side counterpart of the agentfs audit ledger: the workspace
-- audit answers "who touched files", this one answers "what did the
-- materializer/dispatcher do and why did a ticket fail" (docs/designs/0003,
-- T5 AC: 克隆失败 → 工单转 failed + 审计).

CREATE TABLE audit (
    id         BIGSERIAL   PRIMARY KEY,   -- insertion order
    ticket_id  TEXT        NOT NULL,
    event      TEXT        NOT NULL,
    detail     JSONB       NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_ticket_idx ON audit (ticket_id, id);
