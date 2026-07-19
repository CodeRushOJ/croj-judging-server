ALTER TABLE t_external_job
    ADD COLUMN lease_token BINARY(32) NULL AFTER worker_id;
-- migrate:split
ALTER TABLE t_external_job_attempt
    ADD COLUMN tenant_id BIGINT UNSIGNED NULL AFTER id,
    ADD COLUMN lease_token BINARY(32) NULL AFTER worker_id;
-- migrate:split
UPDATE t_external_job_attempt AS attempt_row
JOIN t_external_job AS job ON job.id = attempt_row.job_id
SET attempt_row.tenant_id = job.tenant_id
WHERE attempt_row.tenant_id IS NULL;
-- migrate:split
ALTER TABLE t_external_job_attempt
    MODIFY COLUMN tenant_id BIGINT UNSIGNED NOT NULL,
    ADD UNIQUE KEY uk_external_attempt_id_tenant (id, tenant_id),
    ADD KEY idx_external_attempt_tenant (tenant_id, started_at, id),
    DROP FOREIGN KEY fk_external_attempt_job,
    ADD CONSTRAINT fk_external_attempt_job_tenant
        FOREIGN KEY (job_id, tenant_id) REFERENCES t_external_job(id, tenant_id);
