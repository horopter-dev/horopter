-- Deferred from 0004: nothing provisioned a tenant row on write until Task 5 made tenant
-- credential-derived, and enforcing this earlier rejected every append for a tenant the
-- backfill had not created. Every write now goes through a credential whose tenant_id
-- already references tenant(id), so a written tenant provably exists.
ALTER TABLE event  ADD CONSTRAINT event_tenant_fk  FOREIGN KEY (tenant) REFERENCES tenant (id);
ALTER TABLE change ADD CONSTRAINT change_tenant_fk FOREIGN KEY (tenant) REFERENCES tenant (id);
