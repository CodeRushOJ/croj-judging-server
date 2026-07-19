package external

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestEmbeddedMigrationsDefineTheCompleteJudgeOwnedSchema(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 3 || migrations[0].Version != 1 || migrations[0].Name != "initial_external_judge" || migrations[1].Version != 2 || migrations[1].Name != "external_bundle_ready" || migrations[2].Version != 3 || migrations[2].Name != "durable_job_fencing" {
		t.Fatalf("migrations = %+v", migrations)
	}
	if len(migrations[0].Checksum) != 64 {
		t.Fatalf("checksum = %q", migrations[0].Checksum)
	}
	if _, err := hex.DecodeString(migrations[0].Checksum); err != nil {
		t.Fatalf("checksum is not lowercase SHA-256: %v", err)
	}

	sql := strings.ToLower(migrations[0].SQL)
	for _, table := range []string{
		"t_external_tenant",
		"t_external_api_key",
		"t_external_callback",
		"t_external_bundle",
		"t_external_source_object",
		"t_external_job",
		"t_external_job_attempt",
		"t_external_idempotency",
		"t_external_webhook_outbox",
	} {
		if !strings.Contains(sql, "create table if not exists "+table) {
			t.Errorf("migration does not create %s", table)
		}
	}
	for _, contract := range []string{
		"unique key uk_external_api_key_prefix",
		"unique key uk_external_bundle_tenant_digest",
		"unique key uk_external_idempotency_tenant_scope_key",
		"check (status in ('queued','running','succeeded','failed','cancelled'))",
		"check (source_size_bytes > 0)",
		"check (attempt_no > 0)",
		"unique key uk_external_bundle_id_tenant (id, tenant_id)",
		"unique key uk_external_source_id_tenant (id, tenant_id)",
		"unique key uk_external_callback_id_tenant (id, tenant_id)",
		"unique key uk_external_job_id_tenant (id, tenant_id)",
		"foreign key (bundle_id, tenant_id) references t_external_bundle(id, tenant_id)",
		"foreign key (source_object_id, tenant_id) references t_external_source_object(id, tenant_id)",
		"foreign key (callback_id, tenant_id) references t_external_callback(id, tenant_id)",
		"foreign key (job_id, tenant_id) references t_external_job(id, tenant_id)",
	} {
		if !strings.Contains(sql, contract) {
			t.Errorf("migration is missing contract %q", contract)
		}
	}
	if strings.Contains(sql, "source_code") || strings.Contains(sql, "api_key_plaintext") || strings.Contains(sql, "callback_secret_plaintext") {
		t.Fatal("migration must not persist source or credential plaintext")
	}
	publicationSQL := strings.ToLower(migrations[1].SQL)
	for _, contract := range []string{"publication_status", "staging_object_key", "publish_lease_token", "publish_lease_until", "publish_attempt_count", "publish_next_attempt_at", "publish_last_error_code", "publish_abandoned_at", "ready_at"} {
		if !strings.Contains(publicationSQL, contract) {
			t.Errorf("bundle publication migration is missing %s", contract)
		}
	}
	if strings.Contains(publicationSQL, "update t_external_bundle") || strings.Contains(publicationSQL, "set ready_at = created_at") {
		t.Fatal("legacy bundle rows must not be promoted to READY without remote verification")
	}
}

func TestDurableJobFencingMigrationAddsTenantBoundLeaseTokens(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(migrations[2].SQL)
	for _, contract := range []string{
		"add column lease_token binary(32)",
		"add column tenant_id bigint unsigned",
		"unique key uk_external_attempt_id_tenant (id, tenant_id)",
		"foreign key (job_id, tenant_id) references t_external_job(id, tenant_id)",
	} {
		if !strings.Contains(sql, contract) {
			t.Errorf("migration is missing contract %q", contract)
		}
	}
}

func TestMigrationStatementsAreExplicitAndReplaySafe(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	statements, err := splitMigrationStatements(migrations[0].SQL)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) < 9 {
		t.Fatalf("statement count = %d", len(statements))
	}
	for _, statement := range statements {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(statement)), "create table if not exists") {
			t.Fatalf("migration statement is not replay-safe DDL: %s", statement)
		}
	}
}

func TestMigrationParserRejectsAmbiguousOrEmptyStatements(t *testing.T) {
	for name, sql := range map[string]string{
		"empty":         "  \n",
		"empty section": "CREATE TABLE x(id INT);\n-- migrate:split\n  ",
		"raw delimiter": "CREATE TABLE x(id INT); CREATE TABLE y(id INT);",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := splitMigrationStatements(sql); err == nil {
				t.Fatal("expected migration parser rejection")
			}
		})
	}
}

func TestApplyMigrationsUsesAnAdvisoryLockAndRecordsChecksums(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	connection := &migrationConnectionStub{}
	if err := applyMigrations(context.Background(), connection, migrations); err != nil {
		t.Fatal(err)
	}
	if !connection.locked || !connection.released {
		t.Fatalf("migration lock lifecycle: locked=%v released=%v", connection.locked, connection.released)
	}
	if len(connection.executions) < 19 {
		t.Fatalf("executions = %d, want all three migrations", len(connection.executions))
	}
	if !strings.Contains(strings.ToLower(connection.executions[0].query), "create table if not exists t_judge_schema_history") {
		t.Fatalf("first execution = %s", connection.executions[0].query)
	}
	last := connection.executions[len(connection.executions)-1]
	if !strings.Contains(strings.ToLower(last.query), "insert into t_judge_schema_history") || fmt.Sprint(last.arguments) != fmt.Sprint([]any{3, "durable_job_fencing", migrations[2].Checksum}) {
		t.Fatalf("history execution = %#v", last)
	}
}

func TestApplyMigrationsRejectsChecksumDriftAndStillReleasesTheLock(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	connection := &migrationConnectionStub{history: [][2]any{{1, "different-checksum"}}}
	err = applyMigrations(context.Background(), connection, migrations)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %v", err)
	}
	if !connection.released {
		t.Fatal("migration lock was not released after checksum rejection")
	}
	if len(connection.executions) != 1 {
		t.Fatalf("schema DDL ran after checksum drift: %#v", connection.executions)
	}
}

type migrationExecution struct {
	query     string
	arguments []any
}

type migrationConnectionStub struct {
	locked     bool
	released   bool
	history    [][2]any
	executions []migrationExecution
}

func (connection *migrationConnectionStub) QueryRowContext(_ context.Context, query string, _ ...any) rowScanner {
	if strings.Contains(query, "GET_LOCK") {
		connection.locked = true
		return integerRow(1)
	}
	if strings.Contains(query, "RELEASE_LOCK") {
		connection.released = true
		return integerRow(1)
	}
	return errorRow{err: fmt.Errorf("unexpected row query %q", query)}
}

func (connection *migrationConnectionStub) QueryContext(context.Context, string, ...any) (rowsScanner, error) {
	return &historyRows{values: connection.history}, nil
}

func (connection *migrationConnectionStub) ExecContext(_ context.Context, query string, arguments ...any) (sql.Result, error) {
	connection.executions = append(connection.executions, migrationExecution{query: query, arguments: arguments})
	return stubResult(1), nil
}

type integerRow int

func (row integerRow) Scan(destinations ...any) error {
	if len(destinations) != 1 {
		return fmt.Errorf("unexpected destination count")
	}
	value, ok := destinations[0].(*int)
	if !ok {
		return fmt.Errorf("unexpected destination type")
	}
	*value = int(row)
	return nil
}

type errorRow struct{ err error }

func (row errorRow) Scan(...any) error { return row.err }

type historyRows struct {
	values [][2]any
	index  int
}

func (rows *historyRows) Next() bool { return rows.index < len(rows.values) }
func (rows *historyRows) Scan(destinations ...any) error {
	if len(destinations) != 2 {
		return fmt.Errorf("unexpected history destination count")
	}
	version, versionOK := destinations[0].(*int)
	checksum, checksumOK := destinations[1].(*string)
	if !versionOK || !checksumOK {
		return fmt.Errorf("unexpected history destination types")
	}
	*version = rows.values[rows.index][0].(int)
	*checksum = rows.values[rows.index][1].(string)
	rows.index++
	return nil
}
func (rows *historyRows) Err() error   { return nil }
func (rows *historyRows) Close() error { return nil }

type stubResult int64

func (result stubResult) LastInsertId() (int64, error) { return int64(result), nil }
func (result stubResult) RowsAffected() (int64, error) { return int64(result), nil }
