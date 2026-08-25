-- Grant delegation_app the permissions it needs on the outbox_events table.
-- outbox_events is created by platform-events' own migration (not by 000001)
-- so it is absent from the original GRANT block. Without this the outbox
-- runner (SELECT/UPDATE) and the application's INSERT both fail with
-- "permission denied for table outbox_events" (SQLSTATE 42501).
GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_events TO delegation_app;
