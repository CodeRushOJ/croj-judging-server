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
SELECT
    'hhhhhhhhhhhhhhhhhhhhhhhhhh',
    tenant_a.id,
    bundle_b.id,
    source_b.id,
    callback_b.id,
    'QUEUED',
    'cpp20',
    UNHEX(REPEAT('66', 32))
FROM t_external_tenant AS tenant_a
JOIN t_external_bundle AS bundle_b
  ON bundle_b.external_id = 'cccccccccccccccccccccccccc'
JOIN t_external_source_object AS source_b
  ON source_b.external_id = 'dddddddddddddddddddddddddd'
JOIN t_external_callback AS callback_b
  ON callback_b.external_id = 'eeeeeeeeeeeeeeeeeeeeeeeeee'
WHERE tenant_a.external_id = 'aaaaaaaaaaaaaaaaaaaaaaaaaa';
