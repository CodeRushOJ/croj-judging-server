-- migrate:replay-errors 1060
ALTER TABLE t_external_callback
    ADD COLUMN secret_nonce BINARY(12) NULL AFTER secret_ciphertext;
-- migrate:split
UPDATE t_external_callback
SET disabled_at = COALESCE(disabled_at, CURRENT_TIMESTAMP(3))
WHERE secret_nonce IS NULL
   OR OCTET_LENGTH(secret_ciphertext) <= 16
   OR secret_key_version = 0;
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_callback
    ADD CONSTRAINT chk_external_callback_active_cipher
        CHECK (
            disabled_at IS NOT NULL OR
            (secret_nonce IS NOT NULL AND OCTET_LENGTH(secret_nonce) = 12 AND
             OCTET_LENGTH(secret_ciphertext) > 16 AND secret_key_version > 0)
        );
-- migrate:split
-- migrate:replay-errors 1091,3821,3940
ALTER TABLE t_external_webhook_outbox
    DROP CHECK chk_external_webhook_status;
-- migrate:split
UPDATE t_external_webhook_outbox
SET status = 'DEAD'
WHERE status = 'FAILED';
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_webhook_outbox
    ADD COLUMN payload_body MEDIUMBLOB NULL AFTER payload_json;
-- migrate:split
UPDATE t_external_webhook_outbox
SET payload_body = CAST(payload_json AS CHAR CHARACTER SET utf8mb4)
WHERE payload_body IS NULL;
-- migrate:split
ALTER TABLE t_external_webhook_outbox
    MODIFY COLUMN payload_body MEDIUMBLOB NOT NULL;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_webhook_outbox
    ADD COLUMN worker_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER status;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_webhook_outbox
    ADD COLUMN lease_token BINARY(32) NULL AFTER worker_id;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_webhook_outbox
    ADD COLUMN lease_until DATETIME(3) NULL AFTER lease_token;
-- migrate:split
-- migrate:replay-errors 1060
ALTER TABLE t_external_webhook_outbox
    ADD COLUMN dead_at DATETIME(3) NULL AFTER delivered_at;
-- migrate:split
DROP TEMPORARY TABLE IF EXISTS t_external_webhook_job_uniqueness_guard;
-- migrate:split
CREATE TEMPORARY TABLE t_external_webhook_job_uniqueness_guard (
    job_id BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (job_id)
) ENGINE=InnoDB;
-- migrate:split
INSERT INTO t_external_webhook_job_uniqueness_guard(job_id)
SELECT job_id FROM t_external_webhook_outbox;
-- migrate:split
DROP TEMPORARY TABLE t_external_webhook_job_uniqueness_guard;
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_webhook_outbox
    ADD UNIQUE KEY uk_external_webhook_job (job_id);
-- migrate:split
-- migrate:replay-errors 1091
ALTER TABLE t_external_webhook_outbox
    DROP INDEX idx_external_webhook_delivery;
-- migrate:split
-- migrate:replay-errors 1061
ALTER TABLE t_external_webhook_outbox
    ADD KEY idx_external_webhook_delivery(status, next_attempt_at, lease_until, tenant_id, id);
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_webhook_outbox
    ADD CONSTRAINT chk_external_webhook_status
        CHECK (status IN ('PENDING','DELIVERING','DELIVERED','DEAD'));
-- migrate:split
-- migrate:replay-errors 3822
ALTER TABLE t_external_webhook_outbox
    ADD CONSTRAINT chk_external_webhook_active_lease
        CHECK (
            (status = 'DELIVERING' AND worker_id IS NOT NULL AND lease_token IS NOT NULL AND lease_until IS NOT NULL) OR
            (status <> 'DELIVERING' AND worker_id IS NULL AND lease_token IS NULL AND lease_until IS NULL)
        );
