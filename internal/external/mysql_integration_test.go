package external

import (
	"context"
	"database/sql"
	"os"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, database); err != nil {
		t.Fatalf("migration replay: %v", err)
	}
	var versionCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM t_judge_schema_history WHERE version IN (1, 2, 3)").Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 3 {
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
}
