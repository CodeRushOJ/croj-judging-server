INSERT INTO t_external_tenant(external_id, name, status, policy_json)
VALUES (
    'aaaaaaaaaaaaaaaaaaaaaaaaaa',
    'Schema Fixture Tenant A',
    'ACTIVE',
    JSON_OBJECT(
        'maxQueuedJobs', 100,
        'maxRunningJobs', 4,
        'maxSourceBytes', 1048576,
        'maxRetainedBundles', 200,
        'dailyExecutionMillis', 3600000,
        'maxInfrastructureTries', 3
    )
);

INSERT INTO t_external_tenant(external_id, name, status, policy_json)
VALUES (
    'bbbbbbbbbbbbbbbbbbbbbbbbbb',
    'Schema Fixture Tenant B',
    'ACTIVE',
    JSON_OBJECT(
        'maxQueuedJobs', 100,
        'maxRunningJobs', 4,
        'maxSourceBytes', 1048576,
        'maxRetainedBundles', 200,
        'dailyExecutionMillis', 3600000,
        'maxInfrastructureTries', 3
    )
);

SET @tenant_b = (
    SELECT id
    FROM t_external_tenant
    WHERE external_id = 'bbbbbbbbbbbbbbbbbbbbbbbbbb'
);

INSERT INTO t_external_bundle(
    external_id,
    tenant_id,
    sha256,
    object_key,
    size_bytes,
    case_count,
    manifest_version,
    manifest_json
)
VALUES (
    'cccccccccccccccccccccccccc',
    @tenant_b,
    UNHEX(REPEAT('11', 32)),
    'ci/schema/bundle',
    1,
    1,
    1,
    JSON_OBJECT('schemaVersion', 1)
);
SET @bundle_b = LAST_INSERT_ID();

INSERT INTO t_external_source_object(
    external_id,
    tenant_id,
    object_key,
    source_sha256,
    source_size_bytes,
    encryption_key_version,
    encryption_nonce
)
VALUES (
    'dddddddddddddddddddddddddd',
    @tenant_b,
    'ci/schema/source',
    UNHEX(REPEAT('22', 32)),
    1,
    1,
    UNHEX(REPEAT('33', 12))
);
SET @source_b = LAST_INSERT_ID();

INSERT INTO t_external_callback(
    external_id,
    tenant_id,
    destination_url,
    allowed_host,
    allowed_port,
    secret_ciphertext,
    secret_key_version
)
VALUES (
    'eeeeeeeeeeeeeeeeeeeeeeeeee',
    @tenant_b,
    'https://schema-gate.invalid/callback',
    'schema-gate.invalid',
    443,
    UNHEX('44'),
    1
);
SET @callback_b = LAST_INSERT_ID();

INSERT INTO t_external_job(
    external_id,
    tenant_id,
    bundle_id,
    source_object_id,
    callback_id,
    status,
    language_id,
    request_hash
)
VALUES (
    'ffffffffffffffffffffffffff',
    @tenant_b,
    @bundle_b,
    @source_b,
    @callback_b,
    'QUEUED',
    'cpp20',
    UNHEX(REPEAT('55', 32))
);
SET @job_b = LAST_INSERT_ID();

INSERT INTO t_external_webhook_outbox(
    event_id,
    tenant_id,
    job_id,
    callback_id,
    event_type,
    payload_json,
    expires_at
)
VALUES (
    'gggggggggggggggggggggggggg',
    @tenant_b,
    @job_b,
    @callback_b,
    'job.completed',
    JSON_OBJECT('schemaGate', TRUE),
    DATE_ADD(NOW(3), INTERVAL 1 DAY)
);

SELECT CONCAT_WS(
    '|',
    'same-tenant-ok',
    (SELECT COUNT(*) FROM t_external_job
     WHERE external_id = 'ffffffffffffffffffffffffff'),
    (SELECT COUNT(*) FROM t_external_webhook_outbox
     WHERE event_id = 'gggggggggggggggggggggggggg')
);
