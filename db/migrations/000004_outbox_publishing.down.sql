DROP INDEX IF EXISTS ix_outbox_aggregate_unpublished;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS last_error;
