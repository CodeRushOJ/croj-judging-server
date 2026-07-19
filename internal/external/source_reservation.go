package external

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SweepSourceReservations reconciles outcome-ambiguous object writes. Every
// source object is durably reserved before upload. Once the reservation ages
// past the caller's safety window, a DB reference is authoritative: referenced
// objects are retained, while unreferenced private objects are deleted.
func (repository *MySQLJobRepository) SweepSourceReservations(
	ctx context.Context,
	minimumAge time.Duration,
	limit int,
) (int, error) {
	if repository == nil || minimumAge <= 0 || minimumAge > 7*24*time.Hour || limit < 1 || limit > 1000 {
		return 0, ErrExternalJobInvalid
	}
	reaped := 0
	for reaped < limit {
		tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return reaped, repositoryUnavailable("begin source reservation sweep", err)
		}
		var objectKey string
		err = tx.QueryRowContext(ctx, `
SELECT object_key FROM t_external_source_reservation
WHERE lease_until <= CURRENT_TIMESTAMP(3)
  AND created_at <= CURRENT_TIMESTAMP(3) - INTERVAL ? MICROSECOND
ORDER BY created_at, object_key LIMIT 1 FOR UPDATE SKIP LOCKED`, minimumAge.Microseconds()).Scan(&objectKey)
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return reaped, nil
		}
		if err != nil {
			_ = tx.Rollback()
			return reaped, repositoryUnavailable("lock source reservation", err)
		}
		var referenced int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM t_external_source_object WHERE object_key = ?", objectKey).Scan(&referenced); err != nil {
			_ = tx.Rollback()
			return reaped, repositoryUnavailable("check source reservation ownership", err)
		}
		if referenced == 0 {
			if err := repository.sourceObjects.Delete(ctx, objectKey); err != nil {
				_ = tx.Rollback()
				return reaped, fmt.Errorf("%w: sweep encrypted source object", ErrExternalJobUnavailable)
			}
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM t_external_source_reservation WHERE object_key = ?", objectKey); err != nil {
			_ = tx.Rollback()
			return reaped, repositoryUnavailable("delete source reservation", err)
		}
		if err := tx.Commit(); err != nil {
			return reaped, repositoryUnavailable("commit source reservation sweep", err)
		}
		reaped++
	}
	return reaped, nil
}
