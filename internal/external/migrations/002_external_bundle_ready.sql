ALTER TABLE t_external_bundle
    ADD COLUMN ready_at DATETIME(3) NULL AFTER created_at;
-- migrate:split
UPDATE t_external_bundle
SET ready_at = created_at
WHERE ready_at IS NULL;
