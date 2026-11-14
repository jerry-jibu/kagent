-- 0003_decisions — Eval decision records (T3, docs/designs/0001 §3 Eval/
-- Decision rows and §7 门在编排器).
--
-- decisions is the evaluation audit trail: one row per evaluation attempt
-- of a trigger. Gate denials (outcome=denied, judged in deterministic code
-- before any evaluator invoke), the evaluator's structured Decisions
-- (accept/stop/ignore/unavailable), and code-validation rejections of
-- forged evaluator output (rejected) all land here, so "what happened to
-- trigger X" is answerable from the store alone.
--
-- strength and policy_version always carry the GATE-verified values — never
-- an agent's claim (docs/designs/0001 §7: 消息内容是输入不是权限). The gate
-- denial carries no strength because the admission table refused the sender.
--
-- outcome=unavailable rows are the retryable records (docs/designs/0001 §8
-- failure semantics): re-evaluation continues until a terminal outcome
-- (accept/stop/ignore/denied/rejected) exists for the trigger.

CREATE TABLE decisions (
    id               BIGSERIAL   PRIMARY KEY,
    source           TEXT        NOT NULL,
    deterministic_id TEXT        NOT NULL,
    outcome          TEXT        NOT NULL
                      CHECK (outcome IN ('accept', 'stop', 'ignore', 'unavailable',
                                         'denied', 'rejected')),
    strength         TEXT        NOT NULL DEFAULT '',
    policy_version   TEXT        NOT NULL CHECK (policy_version <> ''),
    reason           TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Every non-denied decision ran under a gate-allowed strength; a denial
    -- carries none.
    CHECK (outcome = 'denied' OR strength IN ('explicit', 'inferred'))
);

CREATE INDEX decisions_trigger_idx ON decisions (source, deterministic_id, id);

-- At most one accept per trigger: the invariant Ticket creation (T4) keys
-- off, enforced even under racing retries.
CREATE UNIQUE INDEX decisions_accept_once_idx
    ON decisions (source, deterministic_id)
    WHERE outcome = 'accept';
