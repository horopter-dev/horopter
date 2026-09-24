-- The lifecycle assertion is a property of the event, and core.Fold reads it, so it has to
-- be in the log: a fold rule reading a value that lives only in the request makes the
-- projection stop being a function of the log. That is 0006's lesson and the reason this is
-- a migration rather than a handler change.

-- DEFAULT '' rather than NULL: absent means "the producer said nothing", which is a real and
-- common value rather than a missing one. Every event stored before this migration said
-- nothing, so the type-suffix convention reproduces its current fold exactly.
ALTER TABLE event ADD COLUMN lifecycle TEXT NOT NULL DEFAULT '';

-- The vocabulary is closed, for the same reason change_status_vocabulary is (0002): a value
-- outside it folds to a status no filter can select.
--
-- Two spellings of "no transition", and they are different facts. '' is ABSENCE - the
-- producer said nothing, so the type suffix decides. 'none' is an ASSERTION that nothing
-- happened, which overrides the suffix and is the only way a producer whose types end in
-- .started can record one as a point.
--
-- occurred and in_progress are absent deliberately: an event asserts a transition, it does
-- not assert the change's state.
ALTER TABLE event ADD CONSTRAINT event_lifecycle_vocabulary
    CHECK (lifecycle IN ('', 'none', 'started', 'completed', 'failed',
                         'rolled_back', 'cancelled'));
