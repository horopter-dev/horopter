-- These are 0004's foreign keys, in a file of their own only because they were written
-- after it. They are not deferred in any operational sense: applyPendingMigrations runs
-- every pending file inside one transaction, so an upgrade applies 0004 and 0005
-- indivisibly and no window exists in which the columns are unconstrained.
--
-- What makes them satisfiable is 0004's backfill, which turns every existing tenant string
-- into a tenant row, plus the inversion that followed it: every write now derives its tenant
-- from a credential whose tenant_id already references tenant(id), so a written tenant
-- provably exists.
ALTER TABLE event  ADD CONSTRAINT event_tenant_fk  FOREIGN KEY (tenant) REFERENCES tenant (id);
ALTER TABLE change ADD CONSTRAINT change_tenant_fk FOREIGN KEY (tenant) REFERENCES tenant (id);
