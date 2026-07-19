ALTER TABLE t_external_bundle
    ADD COLUMN staging_object_key VARCHAR(1024) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER object_key,
    ADD COLUMN publication_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'PENDING' AFTER staging_object_key,
    ADD COLUMN publish_lease_token CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER publication_status,
    ADD COLUMN publish_lease_until DATETIME(3) NULL AFTER publish_lease_token,
    ADD COLUMN publish_attempt_count INT UNSIGNED NOT NULL DEFAULT 0 AFTER publish_lease_until,
    ADD COLUMN publish_next_attempt_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) AFTER publish_attempt_count,
    ADD COLUMN publish_last_error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER publish_next_attempt_at,
    ADD COLUMN publish_abandoned_at DATETIME(3) NULL AFTER publish_last_error_code,
    ADD COLUMN ready_at DATETIME(3) NULL AFTER publish_abandoned_at,
    ADD KEY idx_external_bundle_publication(publication_status, publish_next_attempt_at, publish_lease_until, id),
    ADD CONSTRAINT chk_external_bundle_publication_status CHECK (publication_status IN ('PENDING','PUBLISHING','READY','ABANDONED')),
    ADD CONSTRAINT chk_external_bundle_ready_visibility CHECK (
        (publication_status = 'READY' AND ready_at IS NOT NULL)
        OR (publication_status <> 'READY' AND ready_at IS NULL)
    ),
    ADD CONSTRAINT chk_external_bundle_publish_lease CHECK (
        (publication_status = 'PUBLISHING' AND publish_lease_token IS NOT NULL AND publish_lease_until IS NOT NULL)
        OR (publication_status <> 'PUBLISHING' AND publish_lease_token IS NULL AND publish_lease_until IS NULL)
    );
