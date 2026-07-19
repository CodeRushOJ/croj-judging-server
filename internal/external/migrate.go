package external

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationNamePattern = regexp.MustCompile(`^([0-9]{3})_([a-z0-9_]+)\.sql$`)
var migrationReplayErrorsPattern = regexp.MustCompile(`(?m)^-- migrate:replay-errors ([0-9]+(?:,[0-9]+)*)[ \t]*$`)

type Migration struct {
	Version  int
	Name     string
	Checksum string
	SQL      string
}

const migrationLockName = "coderushoj_judge_schema_migrations"

const createHistoryTableSQL = `CREATE TABLE IF NOT EXISTS t_judge_schema_history (
    version INT UNSIGNED NOT NULL,
    name VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    checksum CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

type rowScanner interface {
	Scan(...any) error
}

type rowsScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

type migrationConnection interface {
	QueryRowContext(context.Context, string, ...any) rowScanner
	QueryContext(context.Context, string, ...any) (rowsScanner, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type sqlMigrationConnection struct{ connection *sql.Conn }

func (connection sqlMigrationConnection) QueryRowContext(ctx context.Context, query string, arguments ...any) rowScanner {
	return connection.connection.QueryRowContext(ctx, query, arguments...)
}

func (connection sqlMigrationConnection) QueryContext(ctx context.Context, query string, arguments ...any) (rowsScanner, error) {
	return connection.connection.QueryContext(ctx, query, arguments...)
}

func (connection sqlMigrationConnection) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	return connection.connection.ExecContext(ctx, query, arguments...)
}

func ApplyMigrations(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return fmt.Errorf("migration database is required")
	}
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open dedicated migration connection: %w", err)
	}
	defer connection.Close()
	return applyMigrations(ctx, sqlMigrationConnection{connection: connection}, migrations)
}

type migrationHistoryQueryer interface {
	QueryContext(context.Context, string, ...any) (rowsScanner, error)
}

type migrationHistoryRecord struct {
	version  int
	name     string
	checksum string
}

// ValidateMigrations verifies that the database has exactly the embedded
// schema history. It performs no DDL and is safe for startup/readiness checks.
func ValidateMigrations(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return fmt.Errorf("migration database is required")
	}
	migrations, err := Migrations()
	if err != nil {
		return err
	}
	connection := sqlMigrationDatabase{database: database}
	if err := validateMigrationHistory(ctx, connection, migrations); err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := validateMigrationPostconditions(ctx, connection, migration); err != nil {
			return fmt.Errorf("validate migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

type sqlMigrationDatabase struct{ database *sql.DB }

func (queryer sqlMigrationDatabase) QueryContext(ctx context.Context, query string, arguments ...any) (rowsScanner, error) {
	return queryer.database.QueryContext(ctx, query, arguments...)
}

func (queryer sqlMigrationDatabase) QueryRowContext(ctx context.Context, query string, arguments ...any) rowScanner {
	return queryer.database.QueryRowContext(ctx, query, arguments...)
}

func (queryer sqlMigrationDatabase) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	return queryer.database.ExecContext(ctx, query, arguments...)
}

func validateMigrationHistory(ctx context.Context, queryer migrationHistoryQueryer, migrations []Migration) error {
	if queryer == nil {
		return fmt.Errorf("migration history queryer is required")
	}
	rows, err := queryer.QueryContext(ctx, "SELECT version, name, checksum FROM t_judge_schema_history ORDER BY version")
	if err != nil {
		return fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]migrationHistoryRecord, len(migrations))
	for rows.Next() {
		var record migrationHistoryRecord
		if err := rows.Scan(&record.version, &record.name, &record.checksum); err != nil {
			return fmt.Errorf("scan migration history: %w", err)
		}
		if _, duplicate := applied[record.version]; duplicate {
			return fmt.Errorf("migration history contains duplicate version %d", record.version)
		}
		applied[record.version] = record
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration history: %w", err)
	}
	known := make(map[int]Migration, len(migrations))
	for _, migration := range migrations {
		known[migration.Version] = migration
		record, exists := applied[migration.Version]
		if !exists {
			return fmt.Errorf("migration %d %s is not applied", migration.Version, migration.Name)
		}
		if record.name != migration.Name {
			return fmt.Errorf("migration %d name drift: database=%s embedded=%s", migration.Version, record.name, migration.Name)
		}
		if record.checksum != migration.Checksum {
			return fmt.Errorf("migration %d checksum drift: database=%s embedded=%s", migration.Version, record.checksum, migration.Checksum)
		}
	}
	for version := range applied {
		if _, exists := known[version]; !exists {
			return fmt.Errorf("database contains unknown migration version %d", version)
		}
	}
	return nil
}

func applyMigrations(ctx context.Context, connection migrationConnection, migrations []Migration) (resultErr error) {
	var acquired int
	if err := connection.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", migrationLockName, 30).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if acquired != 1 {
		return fmt.Errorf("acquire migration lock: timed out")
	}
	defer func() {
		releaseContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released int
		if err := connection.QueryRowContext(releaseContext, "SELECT RELEASE_LOCK(?)", migrationLockName).Scan(&released); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("release migration lock: %w", err)
		} else if released != 1 && resultErr == nil {
			resultErr = fmt.Errorf("release migration lock: lock was not owned")
		}
	}()

	if _, err := connection.ExecContext(ctx, createHistoryTableSQL); err != nil {
		return fmt.Errorf("create migration history: %w", err)
	}
	rows, err := connection.QueryContext(ctx, "SELECT version, checksum FROM t_judge_schema_history ORDER BY version")
	if err != nil {
		return fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]string)
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return fmt.Errorf("scan migration history: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate migration history: %w", err)
	}

	known := make(map[int]Migration, len(migrations))
	for _, migration := range migrations {
		known[migration.Version] = migration
		if checksum, exists := applied[migration.Version]; exists && checksum != migration.Checksum {
			return fmt.Errorf("migration %d checksum drift: database=%s embedded=%s", migration.Version, checksum, migration.Checksum)
		}
	}
	for version := range applied {
		if _, exists := known[version]; !exists {
			return fmt.Errorf("database contains unknown migration version %d", version)
		}
	}

	for _, migration := range migrations {
		if _, exists := applied[migration.Version]; exists {
			continue
		}
		statements, err := splitMigrationStatements(migration.SQL)
		if err != nil {
			return fmt.Errorf("parse migration %d: %w", migration.Version, err)
		}
		for statementIndex, statement := range statements {
			if _, err := connection.ExecContext(ctx, statement); err != nil {
				if !isExplicitlyReplayableMigrationError(statement, err) {
					return fmt.Errorf("apply migration %d statement %d: %w", migration.Version, statementIndex+1, err)
				}
			}
		}
		if err := validateMigrationPostconditions(ctx, connection, migration); err != nil {
			return fmt.Errorf("validate migration %d: %w", migration.Version, err)
		}
		if _, err := connection.ExecContext(ctx,
			"INSERT INTO t_judge_schema_history(version, name, checksum) VALUES (?, ?, ?)",
			migration.Version, migration.Name, migration.Checksum); err != nil {
			return fmt.Errorf("record migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

func isExplicitlyReplayableMigrationError(statement string, executionErr error) bool {
	match := migrationReplayErrorsPattern.FindStringSubmatch(statement)
	if match == nil {
		return false
	}
	var mysqlErr *mysqlDriver.MySQLError
	if !errors.As(executionErr, &mysqlErr) {
		return false
	}
	for _, rawNumber := range strings.Split(match[1], ",") {
		number, err := strconv.ParseUint(rawNumber, 10, 16)
		if err == nil && uint16(number) == mysqlErr.Number {
			return true
		}
	}
	return false
}

func validateMigrationPostconditions(ctx context.Context, connection migrationConnection, migration Migration) error {
	var query, description string
	switch {
	case migration.Version == 3 && migration.Name == "durable_job_fencing":
		query = durableFencingSchemaValidationSQL
		description = "durable fencing schema"
	case migration.Version == 4 && migration.Name == "tenant_policy_execution_ceilings":
		query = tenantPolicyCeilingsValidationSQL
		description = "tenant policy execution ceilings"
	case migration.Version == 5 && migration.Name == "durable_webhook_outbox":
		query = durableWebhookSchemaValidationSQL
		description = "durable webhook schema"
	default:
		return nil
	}
	var valid int
	if err := connection.QueryRowContext(ctx, query).Scan(&valid); err != nil {
		return fmt.Errorf("inspect %s: %w", description, err)
	}
	if valid != 1 {
		return fmt.Errorf("%s does not match its required postconditions", description)
	}
	return nil
}

const tenantPolicyCeilingsValidationSQL = `SELECT NOT EXISTS (
    SELECT 1 FROM t_external_tenant
    WHERE NOT JSON_CONTAINS_PATH(policy_json, 'all', '$.maxTimeLimitMillis', '$.maxMemoryLimitMiB')
       OR CAST(JSON_UNQUOTE(JSON_EXTRACT(policy_json, '$.maxTimeLimitMillis')) AS UNSIGNED) = 0
       OR CAST(JSON_UNQUOTE(JSON_EXTRACT(policy_json, '$.maxMemoryLimitMiB')) AS UNSIGNED) = 0
)`

const durableWebhookSchemaValidationSQL = `SELECT
    EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_callback'
          AND column_name = 'secret_nonce' AND column_type = 'binary(12)' AND is_nullable = 'YES'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND column_name = 'payload_body' AND column_type = 'mediumblob' AND is_nullable = 'NO'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND column_name = 'worker_id' AND column_type = 'varchar(128)'
          AND character_set_name = 'ascii' AND collation_name = 'ascii_bin' AND is_nullable = 'YES'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND column_name = 'lease_token' AND column_type = 'binary(32)' AND is_nullable = 'YES'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND column_name = 'lease_until' AND column_type = 'datetime(3)' AND is_nullable = 'YES'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND column_name = 'dead_at' AND column_type = 'datetime(3)' AND is_nullable = 'YES'
    )
    AND COALESCE((
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND index_name = 'uk_external_webhook_job' AND non_unique = 0
    ), '') = 'job_id'
    AND COALESCE((
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 't_external_webhook_outbox'
          AND index_name = 'idx_external_webhook_delivery'
    ), '') = 'status,next_attempt_at,lease_until,tenant_id,id'
    AND EXISTS (
        SELECT 1
        FROM information_schema.table_constraints AS table_constraint
        JOIN information_schema.check_constraints AS check_constraint
          ON check_constraint.constraint_schema = table_constraint.constraint_schema
         AND check_constraint.constraint_name = table_constraint.constraint_name
        WHERE table_constraint.constraint_schema = DATABASE()
          AND table_constraint.table_name = 't_external_callback'
          AND table_constraint.constraint_type = 'CHECK'
          AND table_constraint.constraint_name = 'chk_external_callback_active_cipher'
          AND table_constraint.enforced = 'YES'
          AND REPLACE(REPLACE(LOWER(check_constraint.check_clause), CHAR(96), ''), CHAR(92), '') =
              '((disabled_at is not null) or ((secret_nonce is not null) and (length(secret_nonce) = 12) and (length(secret_ciphertext) > 16) and (secret_key_version > 0)))'
    )
    AND EXISTS (
        SELECT 1
        FROM information_schema.table_constraints AS table_constraint
        JOIN information_schema.check_constraints AS check_constraint
          ON check_constraint.constraint_schema = table_constraint.constraint_schema
         AND check_constraint.constraint_name = table_constraint.constraint_name
        WHERE table_constraint.constraint_schema = DATABASE()
          AND table_constraint.table_name = 't_external_webhook_outbox'
          AND table_constraint.constraint_type = 'CHECK'
          AND table_constraint.constraint_name = 'chk_external_webhook_status'
          AND table_constraint.enforced = 'YES'
          AND REPLACE(REPLACE(LOWER(check_constraint.check_clause), CHAR(96), ''), CHAR(92), '') =
              '(status in (_utf8mb4''pending'',_utf8mb4''delivering'',_utf8mb4''delivered'',_utf8mb4''dead''))'
    )
    AND EXISTS (
        SELECT 1
        FROM information_schema.table_constraints AS table_constraint
        JOIN information_schema.check_constraints AS check_constraint
          ON check_constraint.constraint_schema = table_constraint.constraint_schema
         AND check_constraint.constraint_name = table_constraint.constraint_name
        WHERE table_constraint.constraint_schema = DATABASE()
          AND table_constraint.table_name = 't_external_webhook_outbox'
          AND table_constraint.constraint_type = 'CHECK'
          AND table_constraint.constraint_name = 'chk_external_webhook_active_lease'
          AND table_constraint.enforced = 'YES'
          AND REPLACE(REPLACE(LOWER(check_constraint.check_clause), CHAR(96), ''), CHAR(92), '') =
              '(((status = _utf8mb4''delivering'') and (worker_id is not null) and (lease_token is not null) and (lease_until is not null)) or ((status <> _utf8mb4''delivering'') and (worker_id is null) and (lease_token is null) and (lease_until is null)))'
    )`

const durableFencingSchemaValidationSQL = `SELECT
    EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_job'
          AND column_name = 'lease_token' AND column_type = 'binary(32)' AND is_nullable = 'YES'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_job_attempt'
          AND column_name = 'tenant_id' AND column_type = 'bigint unsigned' AND is_nullable = 'NO'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_job_attempt'
          AND column_name = 'lease_token' AND column_type = 'binary(32)' AND is_nullable = 'YES'
    )
    AND COALESCE((
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 't_external_job_attempt'
          AND index_name = 'uk_external_attempt_id_tenant' AND non_unique = 0
    ), '') = 'id,tenant_id'
    AND COALESCE((
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 't_external_job_attempt'
          AND index_name = 'idx_external_attempt_tenant'
    ), '') = 'tenant_id,started_at,id'
    AND NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE constraint_schema = DATABASE() AND table_name = 't_external_job_attempt'
          AND constraint_name = 'fk_external_attempt_job'
    )
    AND COALESCE((
        SELECT GROUP_CONCAT(CONCAT(column_name, ':', referenced_column_name)
                            ORDER BY ordinal_position SEPARATOR ',')
        FROM information_schema.key_column_usage
        WHERE constraint_schema = DATABASE() AND table_name = 't_external_job_attempt'
          AND constraint_name = 'fk_external_attempt_job_tenant'
          AND referenced_table_name = 't_external_job'
    ), '') = 'job_id:id,tenant_id:tenant_id'
    AND EXISTS (
        SELECT 1
        FROM information_schema.table_constraints AS table_constraint
        JOIN information_schema.check_constraints AS check_constraint
          ON check_constraint.constraint_schema = table_constraint.constraint_schema
         AND check_constraint.constraint_name = table_constraint.constraint_name
        WHERE table_constraint.constraint_schema = DATABASE()
          AND table_constraint.table_name = 't_external_job_attempt'
          AND table_constraint.constraint_type = 'CHECK'
          AND table_constraint.constraint_name = 'chk_external_attempt_active_lease'
          AND table_constraint.enforced = 'YES'
          AND REPLACE(REPLACE(LOWER(check_constraint.check_clause), CHAR(96), ''), CHAR(92), '') =
              '(((status = _utf8mb4''running'') and (lease_token is not null)) or ((status <> _utf8mb4''running'') and (lease_token is null)))'
    )
    AND EXISTS (
        SELECT 1
        FROM information_schema.table_constraints AS table_constraint
        JOIN information_schema.check_constraints AS check_constraint
          ON check_constraint.constraint_schema = table_constraint.constraint_schema
         AND check_constraint.constraint_name = table_constraint.constraint_name
        WHERE table_constraint.constraint_schema = DATABASE()
          AND table_constraint.table_name = 't_external_job'
          AND table_constraint.constraint_type = 'CHECK'
          AND table_constraint.constraint_name = 'chk_external_job_active_lease'
          AND table_constraint.enforced = 'YES'
          AND REPLACE(REPLACE(LOWER(check_constraint.check_clause), CHAR(96), ''), CHAR(92), '') =
              '(((status = _utf8mb4''running'') and (attempt_no > 0) and (worker_id is not null) and (lease_token is not null) and (lease_until is not null)) or ((status <> _utf8mb4''running'') and (worker_id is null) and (lease_token is null) and (lease_until is null)))'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND engine = 'InnoDB'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND column_name = 'object_key' AND column_type = 'varchar(1024)'
          AND character_set_name = 'ascii' AND collation_name = 'ascii_bin' AND is_nullable = 'NO'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND column_name = 'created_at' AND column_type = 'datetime(3)' AND is_nullable = 'NO'
          AND UPPER(column_default) = 'CURRENT_TIMESTAMP(3)'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND column_name = 'owner_token' AND column_type = 'binary(32)' AND is_nullable = 'NO'
    )
    AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND column_name = 'lease_until' AND column_type = 'datetime(3)' AND is_nullable = 'NO'
    )
    AND COALESCE((
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND index_name = 'PRIMARY' AND non_unique = 0
    ), '') = 'object_key'
    AND COALESCE((
        SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
        FROM information_schema.statistics
        WHERE table_schema = DATABASE() AND table_name = 't_external_source_reservation'
          AND index_name = 'idx_external_source_reservation_created'
    ), '') = 'created_at'`

func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationNamePattern.FindStringSubmatch(filepath.Base(entry.Name()))
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.Atoi(matches[1])
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		if _, exists := seen[version]; exists {
			return nil, fmt.Errorf("duplicate migration version %d", version)
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		if _, err := splitMigrationStatements(string(body)); err != nil {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     matches[2],
			Checksum: hex.EncodeToString(digest[:]),
			SQL:      string(body),
		})
		seen[version] = struct{}{}
	}
	sort.Slice(migrations, func(left, right int) bool { return migrations[left].Version < migrations[right].Version })
	for index, migration := range migrations {
		if migration.Version != index+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 1; found %d at index %d", migration.Version, index)
		}
	}
	return migrations, nil
}

func splitMigrationStatements(script string) ([]string, error) {
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("migration is empty")
	}
	sections := strings.Split(script, "-- migrate:split")
	statements := make([]string, 0, len(sections))
	for index, section := range sections {
		statement := strings.TrimSpace(section)
		if statement == "" {
			return nil, fmt.Errorf("migration statement %d is empty", index+1)
		}
		if strings.Count(statement, ";") != 1 || !strings.HasSuffix(statement, ";") {
			return nil, fmt.Errorf("migration statement %d must contain one trailing semicolon", index+1)
		}
		statements = append(statements, strings.TrimSpace(strings.TrimSuffix(statement, ";")))
	}
	return statements, nil
}
