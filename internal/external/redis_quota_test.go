package external

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type quotaScriptRunnerStub struct {
	result any
	err    error
	keys   []string
	args   []any
}

func (runner *quotaScriptRunnerStub) Eval(_ context.Context, _ string, keys []string, args ...any) (any, error) {
	runner.keys = append([]string(nil), keys...)
	runner.args = append([]any(nil), args...)
	return runner.result, runner.err
}

func TestRedisQuotaUsesTenantOpaqueAtomicBucketAndAllows(t *testing.T) {
	runner := &quotaScriptRunnerStub{result: []any{int64(1), int64(0)}}
	quota, err := NewRedisQuota(runner, "coderushoj")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := quota.Allow(context.Background(), QuotaRequest{
		TenantID: "tenant-7", Kind: QuotaJudgeSubmit, Cost: 1,
		Limit: QuotaLimit{Capacity: 20, RefillPeriod: time.Second},
	})
	if err != nil || !decision.Allowed || decision.RetryAfter != 0 {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
	if len(runner.keys) != 1 || strings.Contains(runner.keys[0], "tenant-7") || !strings.HasPrefix(runner.keys[0], "coderushoj:quota:{") {
		t.Fatalf("quota key must be cluster-safe and opaque: %#v", runner.keys)
	}
	if len(runner.args) != 3 || runner.args[0] != int64(20) || runner.args[1] != int64(1000) || runner.args[2] != int64(1) {
		t.Fatalf("script args=%#v", runner.args)
	}
}

func TestRedisQuotaReturnsServerRetryDelayOnExhaustion(t *testing.T) {
	runner := &quotaScriptRunnerStub{result: []any{int64(0), int64(1751)}}
	quota, err := NewRedisQuota(runner, "coderushoj")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := quota.Allow(context.Background(), QuotaRequest{
		TenantID: "tenant-7", Kind: QuotaBundleUploadBytes, Cost: 1024,
		Limit: QuotaLimit{Capacity: 4096, RefillPeriod: time.Minute},
	})
	if err != nil || decision.Allowed || decision.RetryAfter != 1751*time.Millisecond {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
}

func TestRedisQuotaFailsClosedWhenStateIsUncertain(t *testing.T) {
	for name, runner := range map[string]*quotaScriptRunnerStub{
		"redis error":     {err: errors.New("dial failed")},
		"malformed reply": {result: []any{int64(1)}},
		"overflow retry":  {result: []any{int64(0), int64(1<<63 - 1)}},
	} {
		t.Run(name, func(t *testing.T) {
			quota, err := NewRedisQuota(runner, "coderushoj")
			if err != nil {
				t.Fatal(err)
			}
			_, err = quota.Allow(context.Background(), QuotaRequest{
				TenantID: "tenant-7", Kind: QuotaJudgeSubmit, Cost: 1,
				Limit: QuotaLimit{Capacity: 20, RefillPeriod: time.Second},
			})
			if !errors.Is(err, ErrQuotaUnavailable) || strings.Contains(err.Error(), "dial") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRedisQuotaRejectsTypedNilRunner(t *testing.T) {
	var runner *quotaScriptRunnerStub
	if _, err := NewRedisQuota(runner, "coderushoj"); err == nil {
		t.Fatal("expected typed-nil script runner to be rejected")
	}
}

func TestRedisQuotaRejectsInvalidConfigurationBeforeCallingRedis(t *testing.T) {
	runner := &quotaScriptRunnerStub{}
	quota, err := NewRedisQuota(runner, "coderushoj")
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []QuotaRequest{
		{TenantID: "", Kind: QuotaJudgeSubmit, Cost: 1, Limit: QuotaLimit{Capacity: 1, RefillPeriod: time.Second}},
		{TenantID: "tenant-7", Kind: "unknown", Cost: 1, Limit: QuotaLimit{Capacity: 1, RefillPeriod: time.Second}},
		{TenantID: "tenant-7", Kind: QuotaJudgeSubmit, Cost: 2, Limit: QuotaLimit{Capacity: 1, RefillPeriod: time.Second}},
		{TenantID: "tenant-7", Kind: QuotaJudgeSubmit, Cost: 1, Limit: QuotaLimit{Capacity: maximumRedisQuotaCapacity + 1, RefillPeriod: time.Second}},
		{TenantID: "tenant-7", Kind: QuotaJudgeSubmit, Cost: 1, Limit: QuotaLimit{Capacity: 1, RefillPeriod: maximumRedisQuotaRefillPeriod + time.Millisecond}},
	} {
		if _, err := quota.Allow(context.Background(), request); !errors.Is(err, ErrQuotaInvalid) {
			t.Fatalf("request=%+v error=%v", request, err)
		}
	}
	if runner.keys != nil {
		t.Fatalf("Redis was called for invalid input: %#v", runner.keys)
	}
}
