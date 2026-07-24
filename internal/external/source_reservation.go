package external

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	sourceReservationSweepLease          = 15 * time.Minute
	sourceReservationObjectDeleteTimeout = 30 * time.Second
)

type sourceReservationClaim struct {
	objectKey  string
	ownerToken []byte
}

// SweepSourceReservations reconciles outcome-ambiguous object writes with a
// fenced claim. Database locks are committed before object storage is called;
// finalization succeeds only for the claimant's token. This lets replicas run
// concurrently without holding scarce tenant/reservation locks across MinIO.
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
		claim, found, err := repository.claimSourceReservation(ctx, minimumAge)
		if err != nil {
			return reaped, err
		}
		if !found {
			return reaped, nil
		}
		var referenced int
		if err := repository.database.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM t_external_source_object WHERE object_key = ?", claim.objectKey).Scan(&referenced); err != nil {
			return reaped, repositoryUnavailable("check source reservation ownership", err)
		}
		if referenced == 0 {
			if !validSourceObjectKey(claim.objectKey) {
				return reaped, fmt.Errorf("source reservation object key contract is invalid")
			}
			deleteContext, cancel := context.WithTimeout(ctx, sourceReservationObjectDeleteTimeout)
			err := repository.sourceObjects.Delete(deleteContext, claim.objectKey)
			cancel()
			if err != nil {
				// Retain the claim. Its lease makes the failure retryable without
				// allowing a second replica to delete concurrently.
				return reaped, fmt.Errorf("%w: sweep encrypted source object: %w", ErrExternalJobUnavailable, err)
			}
		}
		result, err := repository.database.ExecContext(ctx, `
DELETE FROM t_external_source_reservation
WHERE object_key = ? AND owner_token = ?`, claim.objectKey, claim.ownerToken)
		if err != nil {
			return reaped, repositoryUnavailable("finalize source reservation", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return reaped, repositoryUnavailable("read source reservation finalization", err)
		}
		if affected == 1 {
			reaped++
		}
	}
	return reaped, nil
}

func (repository *MySQLJobRepository) claimSourceReservation(ctx context.Context, minimumAge time.Duration) (sourceReservationClaim, bool, error) {
	token := make([]byte, 32)
	if _, err := io.ReadFull(repository.random, token); err != nil {
		return sourceReservationClaim{}, false, repositoryUnavailable("generate source reservation claim token", err)
	}
	tx, err := repository.database.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return sourceReservationClaim{}, false, repositoryUnavailable("begin source reservation claim", err)
	}
	defer tx.Rollback()
	var objectKey string
	err = tx.QueryRowContext(ctx, `
SELECT object_key FROM t_external_source_reservation
WHERE lease_until <= CURRENT_TIMESTAMP(3)
  AND created_at <= CURRENT_TIMESTAMP(3) - INTERVAL ? MICROSECOND
ORDER BY created_at, object_key LIMIT 1 FOR UPDATE SKIP LOCKED`, minimumAge.Microseconds()).Scan(&objectKey)
	if errors.Is(err, sql.ErrNoRows) {
		return sourceReservationClaim{}, false, nil
	}
	if err != nil {
		return sourceReservationClaim{}, false, repositoryUnavailable("lock source reservation claim", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE t_external_source_reservation
SET owner_token = ?, lease_until = CURRENT_TIMESTAMP(3) + INTERVAL ? MICROSECOND
WHERE object_key = ? AND lease_until <= CURRENT_TIMESTAMP(3)`, token, sourceReservationSweepLease.Microseconds(), objectKey)
	if err != nil {
		return sourceReservationClaim{}, false, repositoryUnavailable("fence source reservation claim", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return sourceReservationClaim{}, false, repositoryUnavailable("read source reservation claim", err)
	}
	if affected != 1 {
		return sourceReservationClaim{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return sourceReservationClaim{}, false, repositoryUnavailable("commit source reservation claim", err)
	}
	return sourceReservationClaim{objectKey: objectKey, ownerToken: token}, true, nil
}
