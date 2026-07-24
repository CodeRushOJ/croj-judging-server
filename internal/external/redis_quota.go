package external

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var quotaPrefixPattern = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,64}$`)

// QuotaScriptRunner is the narrow adapter needed by RedisQuota. Production
// clients should implement it by returning redis.Client.Eval(...).Result().
type QuotaScriptRunner interface {
	Eval(context.Context, string, []string, ...any) (any, error)
}

type RedisQuota struct {
	runner QuotaScriptRunner
	prefix string
}

type goRedisQuotaScriptRunner struct {
	client redis.Scripter
}

func (runner goRedisQuotaScriptRunner) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return runner.client.Eval(ctx, script, keys, args...).Result()
}

func NewRedisQuotaFromClient(client redis.Scripter, prefix string) (*RedisQuota, error) {
	if quotaDependencyIsNil(client) {
		return nil, fmt.Errorf("Redis quota client is required")
	}
	return NewRedisQuota(goRedisQuotaScriptRunner{client: client}, prefix)
}

func NewRedisQuota(runner QuotaScriptRunner, prefix string) (*RedisQuota, error) {
	if quotaDependencyIsNil(runner) {
		return nil, fmt.Errorf("Redis quota script runner is required")
	}
	prefix = strings.TrimSpace(prefix)
	if !quotaPrefixPattern.MatchString(prefix) {
		return nil, fmt.Errorf("Redis quota key prefix is invalid")
	}
	return &RedisQuota{runner: runner, prefix: prefix}, nil
}

func (quota *RedisQuota) Allow(ctx context.Context, request QuotaRequest) (QuotaDecision, error) {
	if err := validateQuotaRequest(request); err != nil {
		return QuotaDecision{}, err
	}
	result, err := quota.runner.Eval(ctx, redisTokenBucketScript, []string{quota.key(request.TenantID, request.Kind)},
		request.Limit.Capacity, request.Limit.RefillPeriod.Milliseconds(), request.Cost)
	if err != nil {
		return QuotaDecision{}, ErrQuotaUnavailable
	}
	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return QuotaDecision{}, ErrQuotaUnavailable
	}
	allowed, ok := quotaInteger(values[0])
	if !ok || (allowed != 0 && allowed != 1) {
		return QuotaDecision{}, ErrQuotaUnavailable
	}
	retryMilliseconds, ok := quotaInteger(values[1])
	if !ok || retryMilliseconds < 0 || retryMilliseconds > request.Limit.RefillPeriod.Milliseconds() {
		return QuotaDecision{}, ErrQuotaUnavailable
	}
	return QuotaDecision{Allowed: allowed == 1, RetryAfter: time.Duration(retryMilliseconds) * time.Millisecond}, nil
}

func quotaDependencyIsNil(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validateQuotaRequest(request QuotaRequest) error {
	if strings.TrimSpace(request.TenantID) == "" || len(request.TenantID) > 128 {
		return fmt.Errorf("%w: tenant ID is invalid", ErrQuotaInvalid)
	}
	switch request.Kind {
	case QuotaJudgeSubmit, QuotaBundleUploadBytes:
	default:
		return fmt.Errorf("%w: quota kind is invalid", ErrQuotaInvalid)
	}
	if err := request.Limit.Validate(); err != nil {
		return err
	}
	if request.Cost <= 0 || request.Cost > request.Limit.Capacity {
		return fmt.Errorf("%w: quota cost is out of range", ErrQuotaInvalid)
	}
	return nil
}

func (quota *RedisQuota) key(tenantID string, kind QuotaKind) string {
	tenantDigest := sha256.Sum256([]byte(tenantID))
	return quota.prefix + ":quota:{" + hex.EncodeToString(tenantDigest[:]) + "}:" + string(kind)
}

func quotaInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	case []byte:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

const redisTokenBucketScript = `
local capacity = tonumber(ARGV[1])
local period_ms = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local redis_time = redis.call('TIME')
local now_ms = (tonumber(redis_time[1]) * 1000) + math.floor(tonumber(redis_time[2]) / 1000)
local state = redis.call('HMGET', KEYS[1], 'tokens', 'updated_ms')
local tokens = tonumber(state[1]) or capacity
local updated_ms = tonumber(state[2]) or now_ms
local elapsed_ms = math.min(period_ms, math.max(0, now_ms - updated_ms))
tokens = math.min(capacity, tokens + (elapsed_ms * capacity / period_ms))
local allowed = 0
local retry_ms = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry_ms = math.ceil((cost - tokens) * period_ms / capacity)
end
redis.call('HSET', KEYS[1], 'tokens', tostring(tokens), 'updated_ms', tostring(now_ms))
redis.call('PEXPIRE', KEYS[1], math.ceil(math.max(period_ms * 2, retry_ms + period_ms)))
return {allowed, retry_ms}
`
