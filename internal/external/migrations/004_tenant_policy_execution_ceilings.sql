UPDATE t_external_tenant
SET policy_json = JSON_SET(policy_json, '$.maxTimeLimitMillis', 10000)
WHERE NOT JSON_CONTAINS_PATH(policy_json, 'one', '$.maxTimeLimitMillis');
-- migrate:split
UPDATE t_external_tenant
SET policy_json = JSON_SET(policy_json, '$.maxMemoryLimitMiB', 1024)
WHERE NOT JSON_CONTAINS_PATH(policy_json, 'one', '$.maxMemoryLimitMiB');
