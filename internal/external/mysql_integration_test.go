package external

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func openMySQLIntegration(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("JUDGE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("JUDGE_TEST_MYSQL_DSN is not configured")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(32)
	database.SetMaxIdleConns(16)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		t.Fatalf("connect to MySQL integration database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestApplyMigrationsOnMySQL84IsReplaySafe(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	replayContext, cancelReplay := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelReplay()
	if err := ApplyMigrations(replayContext, database); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	var versionCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_judge_schema_history WHERE version IN (1, 2, 3, 4, 5, 6)").Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 6 {
		t.Fatalf("migration versions = %d", versionCount)
	}
	var columnCount int
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema = DATABASE() AND
      (table_name = 't_external_job' AND column_name = 'lease_token' OR
       table_name = 't_external_job_attempt' AND column_name IN ('tenant_id', 'lease_token'))`).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 3 {
		t.Fatalf("durable fencing columns = %d", columnCount)
	}
	var constraintCount int
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.table_constraints
WHERE constraint_schema = DATABASE() AND constraint_type = 'CHECK'
  AND constraint_name IN ('chk_external_job_active_lease', 'chk_external_attempt_active_lease')`).Scan(&constraintCount); err != nil {
		t.Fatal(err)
	}
	if constraintCount != 2 {
		t.Fatalf("active lease constraints = %d", constraintCount)
	}
}

func TestValidateMigrationsRejectsDurableWebhookSchemaDrift(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	defer resetMySQLIntegrationSchema(t, database)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMigrations(ctx, database); err != nil {
		t.Fatalf("valid schema rejected: %v", err)
	}
	if _, err := database.ExecContext(ctx, `ALTER TABLE t_external_callback MODIFY COLUMN secret_nonce BINARY(16) NULL`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMigrations(ctx, database); err == nil {
		t.Fatal("v5 callback nonce schema drift was accepted")
	}
}

func TestValidateMigrationsRejectsExecutionAccountingRetentionContractDrift(t *testing.T) {
	mutations := map[string]string{
		"forced index invisible":    `ALTER TABLE t_external_tenant ALTER INDEX idx_external_tenant_fair_claim INVISIBLE`,
		"forced index prefix":       `ALTER TABLE t_external_tenant DROP INDEX idx_external_tenant_fair_claim, ADD INDEX idx_external_tenant_fair_claim(status(4), last_claimed_at, id)`,
		"attempt default":           `ALTER TABLE t_external_job_attempt ALTER COLUMN consumed_execution_millis SET DEFAULT 1`,
		"daily accounting day null": `ALTER TABLE t_external_execution_daily MODIFY COLUMN accounting_day DATE NULL DEFAULT NULL`,
		"daily foreign key":         `ALTER TABLE t_external_execution_daily DROP FOREIGN KEY fk_external_execution_daily_tenant`,
		"audit external id width":   `ALTER TABLE t_external_retention_audit MODIFY COLUMN job_external_id CHAR(25) CHARACTER SET ascii COLLATE ascii_bin NOT NULL`,
		"audit event type width":    `ALTER TABLE t_external_retention_audit MODIFY COLUMN event_type VARCHAR(31) NOT NULL`,
		"audit check":               `ALTER TABLE t_external_retention_audit DROP CHECK chk_external_retention_audit_event`,
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			database := openMySQLIntegration(t)
			resetMySQLIntegrationSchema(t, database)
			defer resetMySQLIntegrationSchema(t, database)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := ApplyMigrations(ctx, database); err != nil {
				t.Fatal(err)
			}
			if err := ValidateMigrations(ctx, database); err != nil {
				t.Fatalf("valid schema rejected: %v", err)
			}
			if _, err := database.ExecContext(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if err := ValidateMigrations(ctx, database); err == nil {
				t.Fatalf("v6 schema drift %q was accepted", name)
			}
		})
	}
}

func TestValidateMigrationsRejectsExecutionAccountingRetentionForeignKeyActionDrift(t *testing.T) {
	mutations := map[string][]string{
		"daily delete cascade": {
			`ALTER TABLE t_external_execution_daily DROP FOREIGN KEY fk_external_execution_daily_tenant`,
			`ALTER TABLE t_external_execution_daily ADD CONSTRAINT fk_external_execution_daily_tenant
  FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id) ON DELETE CASCADE`,
		},
		"attempt update cascade": {
			`ALTER TABLE t_external_job_attempt DROP FOREIGN KEY fk_external_attempt_job_tenant`,
			`ALTER TABLE t_external_job_attempt ADD CONSTRAINT fk_external_attempt_job_tenant
  FOREIGN KEY (job_id, tenant_id) REFERENCES t_external_job(id, tenant_id) ON UPDATE CASCADE`,
		},
		"source delete cascade": {
			`ALTER TABLE t_external_source_object DROP FOREIGN KEY fk_external_source_tenant`,
			`ALTER TABLE t_external_source_object ADD CONSTRAINT fk_external_source_tenant
  FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id) ON DELETE CASCADE`,
		},
		"source delete set null": {
			`ALTER TABLE t_external_source_object DROP FOREIGN KEY fk_external_source_tenant`,
			`ALTER TABLE t_external_source_object MODIFY COLUMN tenant_id BIGINT UNSIGNED NULL`,
			`ALTER TABLE t_external_source_object ADD CONSTRAINT fk_external_source_tenant
  FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id) ON DELETE SET NULL`,
		},
		"audit update cascade": {
			`ALTER TABLE t_external_retention_audit DROP FOREIGN KEY fk_external_retention_audit_tenant`,
			`ALTER TABLE t_external_retention_audit ADD CONSTRAINT fk_external_retention_audit_tenant
  FOREIGN KEY (tenant_id) REFERENCES t_external_tenant(id) ON UPDATE CASCADE`,
		},
	}
	for name, statements := range mutations {
		t.Run(name, func(t *testing.T) {
			database := openMySQLIntegration(t)
			resetMySQLIntegrationSchema(t, database)
			defer resetMySQLIntegrationSchema(t, database)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := ApplyMigrations(ctx, database); err != nil {
				t.Fatal(err)
			}
			for _, statement := range statements {
				if _, err := database.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateMigrations(ctx, database); err == nil {
				t.Fatalf("foreign key action drift %q was accepted", name)
			}
		})
	}
}

func TestDurableWebhookMigrationUpgradesLegacyRowsWithoutInventingSecrets(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations[:3]); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	jobID := insertLegacyWebhookFixture(t, ctx, database, "aaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccc")
	payload := `{"eventId":"dddddddddddddddddddddddddd","eventType":"judge.job.failed"}`
	if _, err := database.ExecContext(ctx, `
INSERT INTO t_external_webhook_outbox(
    event_id, tenant_id, job_id, callback_id, event_type, payload_json,
    status, next_attempt_at, expires_at
)
SELECT 'dddddddddddddddddddddddddd', job.tenant_id, job.id, job.callback_id,
       'judge.job.failed', ?, 'FAILED', CURRENT_TIMESTAMP(3), DATE_ADD(CURRENT_TIMESTAMP(3), INTERVAL 1 DAY)
FROM t_external_job AS job WHERE job.id = ?`, payload, jobID); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatalf("replay durable webhook migration: %v", err)
	}
	var disabledAt sql.NullTime
	if err := database.QueryRowContext(ctx, `
SELECT callback.disabled_at
FROM t_external_callback AS callback
JOIN t_external_job AS job ON job.callback_id = callback.id
WHERE job.id = ?`, jobID).Scan(&disabledAt); err != nil {
		t.Fatal(err)
	}
	if !disabledAt.Valid {
		t.Fatal("legacy callback without nonce remained enabled")
	}
	var status string
	var body []byte
	if err := database.QueryRowContext(ctx, "SELECT status, payload_body FROM t_external_webhook_outbox WHERE job_id = ?", jobID).Scan(&status, &body); err != nil {
		t.Fatal(err)
	}
	var legacyBody map[string]string
	if err := json.Unmarshal(body, &legacyBody); err != nil {
		t.Fatalf("decode migrated legacy payload: %v", err)
	}
	if status != "DEAD" || legacyBody["eventId"] != "dddddddddddddddddddddddddd" || legacyBody["eventType"] != "judge.job.failed" {
		t.Fatalf("legacy outbox status=%q body=%q", status, body)
	}
}

func TestDurableWebhookMigrationRejectsDuplicateLegacyJobEventsWithoutDeletingThem(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations[:3]); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	jobID := insertLegacyWebhookFixture(t, ctx, database, "eeeeeeeeeeeeeeeeeeeeeeeeee", "ffffffffffffffffffffffffff", "gggggggggggggggggggggggggg")
	for _, eventID := range []string{"hhhhhhhhhhhhhhhhhhhhhhhhhh", "iiiiiiiiiiiiiiiiiiiiiiiiii"} {
		payload := fmt.Sprintf(`{"eventId":%q,"eventType":"judge.job.failed"}`, eventID)
		if _, err := database.ExecContext(ctx, `
INSERT INTO t_external_webhook_outbox(
    event_id, tenant_id, job_id, callback_id, event_type, payload_json,
    status, next_attempt_at, expires_at
)
SELECT ?, job.tenant_id, job.id, job.callback_id, 'judge.job.failed', ?,
       'FAILED', CURRENT_TIMESTAMP(3), DATE_ADD(CURRENT_TIMESTAMP(3), INTERVAL 1 DAY)
FROM t_external_job AS job WHERE job.id = ?`, eventID, payload, jobID); err != nil {
			t.Fatal(err)
		}
	}
	if err := ApplyMigrations(ctx, database); err == nil || !strings.Contains(err.Error(), "migration 5") {
		t.Fatalf("duplicate legacy migration error = %v", err)
	}
	var count int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_external_webhook_outbox WHERE job_id = ?", jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("duplicate legacy events were changed: %d", count)
	}
}

func insertLegacyWebhookFixture(t *testing.T, ctx context.Context, database *sql.DB, tenantExternalID, callbackExternalID, jobExternalID string) int64 {
	t.Helper()
	policy := `{"maxQueuedJobs":4,"maxRunningJobs":1,"maxSourceBytes":1024,"maxRetainedBundles":4,"dailyExecutionMillis":1000,"maxInfrastructureTries":3}`
	tenantResult, err := database.ExecContext(ctx, `INSERT INTO t_external_tenant(external_id, name, status, policy_json) VALUES (?, 'legacy webhook tenant', 'ACTIVE', ?)`, tenantExternalID, policy)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, _ := tenantResult.LastInsertId()
	callbackResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_callback(external_id, tenant_id, destination_url, allowed_host, allowed_port, secret_ciphertext, secret_key_version)
VALUES (?, ?, 'https://oj.example.com/hook', 'oj.example.com', 443, ?, 1)`, callbackExternalID, tenantID, []byte("legacy-ciphertext-longer-than-tag"))
	if err != nil {
		t.Fatal(err)
	}
	callbackID, _ := callbackResult.LastInsertId()
	bundleResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_bundle(external_id, tenant_id, sha256, object_key, size_bytes, case_count, manifest_version, manifest_json, publication_status, ready_at)
VALUES ('jjjjjjjjjjjjjjjjjjjjjjjjjj', ?, UNHEX(SHA2(?, 256)), ?, 1, 1, 1, JSON_OBJECT('schemaVersion', 1), 'READY', CURRENT_TIMESTAMP(3))`, tenantID, jobExternalID, "external/"+jobExternalID+".zip")
	if err != nil {
		t.Fatal(err)
	}
	bundleID, _ := bundleResult.LastInsertId()
	sourceResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_source_object(external_id, tenant_id, object_key, source_sha256, source_size_bytes, encryption_key_version, encryption_nonce)
VALUES ('kkkkkkkkkkkkkkkkkkkkkkkkkk', ?, ?, UNHEX(SHA2(?, 256)), 1, 1, X'000000000000000000000000')`, tenantID, "external/"+jobExternalID+".bin", jobExternalID)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := sourceResult.LastInsertId()
	jobResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_job(external_id, tenant_id, bundle_id, source_object_id, callback_id, status, language_id, request_hash, next_attempt_at, completed_at)
VALUES (?, ?, ?, ?, ?, 'FAILED', 'cpp', UNHEX(SHA2(?, 256)), CURRENT_TIMESTAMP(3), CURRENT_TIMESTAMP(3))`, jobExternalID, tenantID, bundleID, sourceID, callbackID, jobExternalID)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := jobResult.LastInsertId()
	return jobID
}

func TestTenantPolicyExecutionCeilingsMigrationBackfillsMissingValuesAndReplays(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) < 5 {
		t.Fatalf("migration count = %d, want at least 5", len(migrations))
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations[:3]); err != nil {
		t.Fatal(err)
	}

	missingID := "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	existingID := "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := connection.ExecContext(ctx, `
INSERT INTO t_external_tenant(external_id, name, status, policy_json) VALUES
    (?, 'missing ceilings', 'ACTIVE', JSON_OBJECT('maxQueuedJobs', 4)),
    (?, 'existing ceilings', 'ACTIVE', JSON_OBJECT(
        'maxQueuedJobs', 4,
        'maxTimeLimitMillis', 2500,
        'maxMemoryLimitMiB', 384
    ))`, missingID, existingID); err != nil {
		t.Fatal(err)
	}
	statements, err := splitMigrationStatements(migrations[3].SQL)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 {
		t.Fatalf("tenant policy ceilings statements = %d, want 2", len(statements))
	}
	if _, err := connection.ExecContext(ctx, statements[0]); err != nil {
		t.Fatalf("execute committed migration prefix: %v", err)
	}

	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations); err != nil {
		t.Fatalf("resume tenant policy ceilings migration: %v", err)
	}
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations); err != nil {
		t.Fatalf("replay tenant policy ceilings migration: %v", err)
	}

	assertPolicyCeilings := func(externalID string, wantTime, wantMemory int) {
		t.Helper()
		var gotTime, gotMemory int
		if err := connection.QueryRowContext(ctx, `
SELECT policy_json->>'$.maxTimeLimitMillis', policy_json->>'$.maxMemoryLimitMiB'
FROM t_external_tenant WHERE external_id = ?`, externalID).Scan(&gotTime, &gotMemory); err != nil {
			t.Fatal(err)
		}
		if gotTime != wantTime || gotMemory != wantMemory {
			t.Fatalf("tenant %s ceilings = %d/%d, want %d/%d", externalID, gotTime, gotMemory, wantTime, wantMemory)
		}
	}
	assertPolicyCeilings(missingID, 10_000, 1024)
	assertPolicyCeilings(existingID, 2500, 384)
}

func TestDurableFencingMigrationResumesAfterCommittedStatementPrefix(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	statements, err := splitMigrationStatements(migrations[2].SQL)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) < 2 {
		t.Fatalf("durable migration statements = %d", len(statements))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations[:2]); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	committed := len(statements) - 1
	for index := 0; index < committed; index++ {
		if _, err := connection.ExecContext(ctx, statements[index]); err != nil {
			connection.Close()
			t.Fatalf("execute durable statement %d before simulated interruption: %v", index+1, err)
		}
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatalf("resume after %d committed statements: %v", committed, err)
	}
	assertDurableMigrationSchema(t, ctx, database)
}

func TestDurableFencingMigrationRejectsWrongSameNameChecks(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	defer resetMySQLIntegrationSchema(t, database)
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations[:2]); err != nil {
		t.Fatal(err)
	}
	statements, err := splitMigrationStatements(migrations[2].SQL)
	if err != nil {
		t.Fatal(err)
	}
	for index, statement := range statements {
		if strings.Contains(statement, "chk_external_attempt_active_lease") {
			statements = statements[:index]
			break
		}
	}
	for index, statement := range statements {
		if _, err := connection.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare statement %d: %v", index+1, err)
		}
	}
	if _, err := connection.ExecContext(ctx, `ALTER TABLE t_external_job_attempt
ADD CONSTRAINT chk_external_attempt_active_lease CHECK (status <> 'RUNNING' OR lease_token IS NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, `ALTER TABLE t_external_job
ADD CONSTRAINT chk_external_job_active_lease CHECK (
    status <> 'RUNNING' OR
    (worker_id IS NOT NULL AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
)`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations); err == nil || !strings.Contains(err.Error(), "validate migration 3") {
		t.Fatalf("wrong same-name checks migration error=%v", err)
	}
	var recorded int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_judge_schema_history WHERE version = 3").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatalf("invalid migration recorded history rows=%d", recorded)
	}
}

func assertDurableMigrationSchema(t *testing.T, ctx context.Context, database *sql.DB) {
	t.Helper()
	var versionCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_judge_schema_history WHERE version = 3").Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 1 {
		t.Fatalf("durable migration history rows = %d", versionCount)
	}
	var reservationTableCount int
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'`).Scan(&reservationTableCount); err != nil {
		t.Fatal(err)
	}
	if reservationTableCount != 1 {
		t.Fatalf("source reservation tables = %d", reservationTableCount)
	}
}

func TestDurableFencingMigrationRecoversLegacyRunningRows(t *testing.T) {
	database := openMySQLIntegration(t)
	resetMySQLIntegrationSchema(t, database)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations[:2]); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	policy := `{"maxQueuedJobs":4,"maxRunningJobs":1,"maxSourceBytes":1024,"maxRetainedBundles":4,"dailyExecutionMillis":1000,"maxInfrastructureTries":3,"maxTimeLimitMillis":10000,"maxMemoryLimitMiB":1024}`
	tenantResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_tenant(external_id, name, status, policy_json)
VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaa', 'legacy tenant', 'ACTIVE', ?)`, policy)
	if err != nil {
		t.Fatal(err)
	}
	tenantID, _ := tenantResult.LastInsertId()
	bundleResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_bundle(
    external_id, tenant_id, sha256, object_key, size_bytes, case_count,
    manifest_version, manifest_json, publication_status, ready_at
) VALUES ('bbbbbbbbbbbbbbbbbbbbbbbbbb', ?, UNHEX(SHA2('legacy-bundle', 256)),
          'external/legacy.zip', 1, 1, 1, JSON_OBJECT('schemaVersion', 1), 'READY', NOW(3))`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	bundleID, _ := bundleResult.LastInsertId()
	sourceResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_source_object(
    external_id, tenant_id, object_key, source_sha256, source_size_bytes,
    encryption_key_version, encryption_nonce
) VALUES ('cccccccccccccccccccccccccc', ?, 'external/legacy-source.bin',
          UNHEX(SHA2('legacy-source', 256)), 1, 1, X'000000000000000000000000')`, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, _ := sourceResult.LastInsertId()
	jobResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_job(
    external_id, tenant_id, bundle_id, source_object_id, status, language_id,
    request_hash, attempt_no, worker_id, lease_until, next_attempt_at, started_at
) VALUES ('dddddddddddddddddddddddddd', ?, ?, ?, 'RUNNING', 'cpp',
          UNHEX(SHA2('legacy-request', 256)), 1, 'legacy-worker',
          DATE_ADD(NOW(3), INTERVAL 1 HOUR), NOW(3), NOW(3))`, tenantID, bundleID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := jobResult.LastInsertId()
	residueJobResult, err := database.ExecContext(ctx, `
INSERT INTO t_external_job(
    external_id, tenant_id, bundle_id, source_object_id, status, language_id,
    request_hash, worker_id, lease_until, next_attempt_at, completed_at
) VALUES ('eeeeeeeeeeeeeeeeeeeeeeeeee', ?, ?, ?, 'FAILED', 'cpp',
          UNHEX(SHA2('legacy-residue-request', 256)), 'stale-worker',
          DATE_ADD(NOW(3), INTERVAL 1 HOUR), NOW(3), NOW(3))`, tenantID, bundleID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	residueJobID, _ := residueJobResult.LastInsertId()
	if _, err := database.ExecContext(ctx, `
INSERT INTO t_external_job_attempt(job_id, attempt_no, worker_id, status, lease_until)
VALUES (?, 1, 'legacy-worker', 'RUNNING', DATE_ADD(NOW(3), INTERVAL 1 HOUR))`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	var jobStatus, jobFailure string
	var workerID, leaseUntil sql.NullString
	if err := database.QueryRowContext(ctx, `
SELECT status, failure_code, worker_id, CAST(lease_until AS CHAR)
FROM t_external_job WHERE id = ?`, jobID).Scan(&jobStatus, &jobFailure, &workerID, &leaseUntil); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "QUEUED" || jobFailure != "MIGRATION_RECLAIM" || workerID.Valid || leaseUntil.Valid {
		t.Fatalf("migrated job status=%s failure=%s worker=%v lease=%v", jobStatus, jobFailure, workerID, leaseUntil)
	}
	var attemptStatus, attemptFailure string
	var attemptTenantID uint64
	if err := database.QueryRowContext(ctx, `
SELECT status, failure_code, tenant_id FROM t_external_job_attempt WHERE job_id = ?`, jobID).
		Scan(&attemptStatus, &attemptFailure, &attemptTenantID); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "EXPIRED" || attemptFailure != "MIGRATION_RECLAIM" || attemptTenantID != uint64(tenantID) {
		t.Fatalf("migrated attempt status=%s failure=%s tenant=%d", attemptStatus, attemptFailure, attemptTenantID)
	}
	var residueWorker, residueLease sql.NullString
	if err := database.QueryRowContext(ctx, `
SELECT worker_id, CAST(lease_until AS CHAR) FROM t_external_job WHERE id = ?`, residueJobID).
		Scan(&residueWorker, &residueLease); err != nil {
		t.Fatal(err)
	}
	if residueWorker.Valid || residueLease.Valid {
		t.Fatalf("non-running migration residue worker=%v lease=%v", residueWorker, residueLease)
	}
	if _, err := database.ExecContext(ctx, `
UPDATE t_external_job SET worker_id = 'stale-worker' WHERE id = ?`, residueJobID); err == nil {
		t.Fatal("non-running job accepted worker residue")
	}
	if _, err := database.ExecContext(ctx, `
UPDATE t_external_job
SET status = 'RUNNING', worker_id = 'worker', lease_token = RANDOM_BYTES(32),
    lease_until = DATE_ADD(CURRENT_TIMESTAMP(3), INTERVAL 1 MINUTE)
WHERE id = ?`, residueJobID); err == nil {
		t.Fatal("attempt-zero job accepted a running lease")
	}
	if _, err := database.ExecContext(ctx, `
UPDATE t_external_job_attempt SET lease_token = RANDOM_BYTES(32) WHERE job_id = ?`, jobID); err == nil {
		t.Fatal("non-running attempt accepted lease token residue")
	}
}

func resetMySQLIntegrationSchema(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, table := range []string{
		"t_external_retention_audit", "t_external_execution_daily", "t_external_source_reservation", "t_external_webhook_outbox", "t_external_job_attempt", "t_external_idempotency",
		"t_external_job", "t_external_source_object", "t_external_callback", "t_external_bundle",
		"t_external_api_key", "t_external_tenant", "t_judge_schema_history",
	} {
		if _, err := database.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
}
