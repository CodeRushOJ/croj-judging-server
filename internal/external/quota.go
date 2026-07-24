package external

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrQuotaUnavailable = errors.New("quota state is unavailable")
	ErrQuotaInvalid     = errors.New("quota request is invalid")
)

const (
	// Redis Lua numbers are IEEE-754 doubles. These bounds keep
	// capacity*refillMilliseconds below 2^53 for exact integer arithmetic.
	maximumRedisQuotaCapacity     = int64(1 << 31)
	maximumRedisQuotaRefillPeriod = time.Hour
)

type QuotaKind string

const (
	QuotaJudgeSubmit       QuotaKind = "judge-submit"
	QuotaBundleUploadBytes QuotaKind = "bundle-upload-bytes"
)

type QuotaLimit struct {
	Capacity     int64
	RefillPeriod time.Duration
}

func (limit QuotaLimit) Validate() error {
	if limit.Capacity <= 0 || limit.Capacity > maximumRedisQuotaCapacity ||
		limit.RefillPeriod < time.Millisecond || limit.RefillPeriod > maximumRedisQuotaRefillPeriod {
		return fmt.Errorf("%w: capacity and refill period are out of range", ErrQuotaInvalid)
	}
	return nil
}

type QuotaRequest struct {
	TenantID string
	Kind     QuotaKind
	Cost     int64
	Limit    QuotaLimit
}

type QuotaDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type Quota interface {
	Allow(context.Context, QuotaRequest) (QuotaDecision, error)
}
