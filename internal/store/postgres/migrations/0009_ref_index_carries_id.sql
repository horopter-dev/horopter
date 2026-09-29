-- The per-ref re-fold reads a ref's events in id order on the append hot path. Carrying
-- id in the index lets that read come back ordered instead of sorting a handful of rows
-- inside the append's transaction, while it holds the ref's advisory lock.
--
-- decisions/2026-09-16-out-of-order-handling-belongs-in-the-projection.md records this as
-- "an optimisation, not a prerequisite"; it became worth taking when the re-fold stopped
-- being a background operation.
--
-- The old index is a strict prefix of the new one, so nothing that used it loses a plan.
DROP INDEX IF EXISTS ix_event_tenant_corr;
CREATE INDEX ix_event_tenant_corr ON event (tenant, correlation_ref, id);
