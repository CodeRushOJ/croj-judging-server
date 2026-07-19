-- migrate:replay-errors 1060
ALTER TABLE t_external_job
    ADD COLUMN lease_token BINARY(32) NULL AFTER worker_id;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_job_attempt
    ADD COLUMN tenant_id BIGINT UNSIGNED NULL AFTER id;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_job_attempt
    ADD COLUMN lease_token BINARY(32) NULL AFTER worker_id;
-- migrate:split
UPDATE t_external_job_attempt AS attempt_row
JOIN t_external_job AS job ON job.id = attempt_row.job_id
SET attempt_row.tenant_id = job.tenant_id
WHERE attempt_row.tenant_id IS NULL;
-- migrate:split
UPDATE t_external_job_attempt
SET status = 'EXPIRED',
    finished_at = COALESCE(finished_at, CURRENT_TIMESTAMP(3)),
    failure_code = 'MIGRATION_RECLAIM'
WHERE status = 'RUNNING';
-- migrate:split
UPDATE t_external_job
SET status = 'QUEUED',
    worker_id = NULL,
    lease_token = NULL,
    lease_until = NULL,
    next_attempt_at = CURRENT_TIMESTAMP(3),
    failure_code = 'MIGRATION_RECLAIM'
WHERE status = 'RUNNING';
-- migrate:split
ALTER TABLE t_external_job_attempt
    MODIFY COLUMN tenant_id BIGINT UNSIGNED NOT NULL;
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_job_attempt
    ADD UNIQUE KEY uk_external_attempt_id_tenant (id, tenant_id);
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_job_attempt
    ADD KEY idx_external_attempt_tenant (tenant_id, started_at, id);
-- migrate:split
-- migrate:replay-errors 1091
ALTER TABLE t_external_job_attempt
    DROP FOREIGN KEY fk_external_attempt_job;
-- migrate:split
-- migrate:replay-errors 1826
ALTER TABLE t_external_job_attempt
    ADD CONSTRAINT fk_external_attempt_job_tenant
        FOREIGN KEY (job_id, tenant_id) REFERENCES t_external_job(id, tenant_id);
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_job_attempt
    ADD CONSTRAINT chk_external_attempt_active_lease
        CHECK (status <> 'RUNNING' OR lease_token IS NOT NULL);
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_job
    ADD CONSTRAINT chk_external_job_active_lease
        CHECK (
            status <> 'RUNNING' OR
            (worker_id IS NOT NULL AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
        );
-- migrate:split
CREATE TABLE IF NOT EXISTS t_external_source_reservation (
    object_key VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
	owner_token BINARY(32) NOT NULL,
	lease_until DATETIME(3) NOT NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (object_key),
    KEY idx_external_source_reservation_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
