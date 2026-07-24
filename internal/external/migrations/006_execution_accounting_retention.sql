-- migrate:replay-errors 1060
ALTER TABLE t_external_tenant
    ADD COLUMN last_claimed_at DATETIME(3) NULL AFTER updated_at;
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_tenant
    ADD KEY idx_external_tenant_fair_claim(status, last_claimed_at, id);
-- migrate:split
-- migrate:replay-errors 1091
ALTER TABLE t_external_job_attempt
    DROP FOREIGN KEY fk_external_attempt_job_tenant;
-- migrate:split
-- migrate:replay-errors 1826
ALTER TABLE t_external_job_attempt
    ADD CONSTRAINT fk_external_attempt_job_tenant
        FOREIGN KEY (job_id, tenant_id) REFERENCES t_external_job(id, tenant_id)
        ON DELETE RESTRICT ON UPDATE RESTRICT;
-- migrate:split
-- migrate:replay-errors 1091
ALTER TABLE t_external_source_object
    DROP FOREIGN KEY fk_external_source_tenant;
-- migrate:split
-- migrate:replay-errors 1826
ALTER TABLE t_external_source_object
    ADD CONSTRAINT fk_external_source_tenant
        FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id)
        ON DELETE RESTRICT ON UPDATE RESTRICT;
-- migrate:split
CREATE TABLE IF NOT EXISTS t_external_execution_daily (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    accounting_day DATE NOT NULL,
    reserved_millis BIGINT UNSIGNED NOT NULL DEFAULT 0,
    consumed_millis BIGINT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uk_external_execution_daily (tenant_id, accounting_day),
    CONSTRAINT fk_external_execution_daily_tenant FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id)
        ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_job_attempt
    ADD COLUMN accounting_day DATE NULL AFTER lease_until;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_job_attempt
    ADD COLUMN reserved_execution_millis BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER accounting_day;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_job_attempt
    ADD COLUMN consumed_execution_millis BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER reserved_execution_millis;
-- migrate:split
UPDATE t_external_job_attempt
SET status = 'EXPIRED', lease_token = NULL, finished_at = COALESCE(finished_at, CURRENT_TIMESTAMP(3)),
    failure_code = 'MIGRATION_ACCOUNTING_RECLAIM', accounting_day = NULL,
    reserved_execution_millis = 0, consumed_execution_millis = 0
WHERE status = 'RUNNING';
-- migrate:split
UPDATE t_external_job
SET status = 'QUEUED', worker_id = NULL, lease_token = NULL, lease_until = NULL,
    next_attempt_at = CURRENT_TIMESTAMP(3), failure_code = NULL
WHERE status = 'RUNNING';
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_job_attempt
    ADD CONSTRAINT chk_external_attempt_accounting
        CHECK (
            (status = 'RUNNING' AND accounting_day IS NOT NULL AND reserved_execution_millis > 0 AND consumed_execution_millis = 0)
            OR (status <> 'RUNNING' AND reserved_execution_millis = 0)
        );
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_job
    ADD KEY idx_external_job_retention(status, completed_at, id);
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_source_object
    ADD COLUMN delete_token BINARY(32) NULL AFTER delete_marked_at;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_source_object
    ADD COLUMN delete_lease_until DATETIME(3) NULL AFTER delete_token;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_source_object
    ADD COLUMN delete_next_attempt_at DATETIME(3) NULL AFTER delete_lease_until;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_source_object
    ADD COLUMN delete_attempt_count INT UNSIGNED NOT NULL DEFAULT 0 AFTER delete_next_attempt_at;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_source_object
    ADD COLUMN delete_last_error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER delete_attempt_count;
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_source_object
    ADD KEY idx_external_source_delete(delete_next_attempt_at, delete_lease_until, deleted_at, id);
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_source_object
    ADD CONSTRAINT chk_external_source_delete_fence
        CHECK (
            (delete_marked_at IS NULL AND delete_token IS NULL AND delete_lease_until IS NULL AND delete_next_attempt_at IS NULL)
            OR (delete_marked_at IS NOT NULL AND delete_token IS NOT NULL AND delete_lease_until IS NOT NULL AND delete_next_attempt_at IS NOT NULL)
        );
-- migrate:split
CREATE TABLE IF NOT EXISTS t_external_retention_audit (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    job_external_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_external_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_external_retention_audit_tenant_time(tenant_id, event_at, id),
    CONSTRAINT fk_external_retention_audit_tenant FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id)
        ON DELETE RESTRICT ON UPDATE RESTRICT,
    CONSTRAINT chk_external_retention_audit_event CHECK (event_type IN ('MARKED','DELETE_RETRY','DELETED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
