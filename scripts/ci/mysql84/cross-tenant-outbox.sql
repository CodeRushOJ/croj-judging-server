INSERT INTO t_external_webhook_outbox(
    event_id,
    tenant_id,
    job_id,
    callback_id,
    event_type,
    payload_json,
    expires_at
)
SELECT
    'iiiiiiiiiiiiiiiiiiiiiiiiii',
    tenant_a.id,
    job_b.id,
    callback_b.id,
    'job.completed',
    JSON_OBJECT('schemaGate', TRUE),
    DATE_ADD(NOW(3), INTERVAL 1 DAY)
FROM t_external_tenant AS tenant_a
JOIN t_external_job AS job_b
  ON job_b.external_id = 'ffffffffffffffffffffffffff'
JOIN t_external_callback AS callback_b
  ON callback_b.external_id = 'eeeeeeeeeeeeeeeeeeeeeeeeee'
WHERE tenant_a.external_id = 'aaaaaaaaaaaaaaaaaaaaaaaaaa';
