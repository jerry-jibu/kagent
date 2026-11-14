-- 0006_stuck — stuck-assignment detection (T6, G7 from the substrate
-- reference review; docs/designs/0009 §4 red line).
--
-- assignments.started_deadline  when a dispatched task must have produced
--                              an outcome (dispatch time + configured
--                              window). NULL = no deadline (undeadlined
--                              dispatches and pre-migration rows never trip
--                              the stuck branch). The Pass scans running
--                              assignments past their deadline whose task is
--                              still non-terminal and marks the ticket
--                              needs-attention — never an automatic replay.

ALTER TABLE assignments ADD COLUMN started_deadline TIMESTAMPTZ;
