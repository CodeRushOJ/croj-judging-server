package external

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisQuotaIsAtomicAcrossReplicas(t *testing.T) {
	address := os.Getenv("REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("REDIS_TEST_ADDR is not configured")
	}
	firstClient := redis.NewClient(&redis.Options{Addr: address})
	secondClient := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() {
		_ = firstClient.Close()
		_ = secondClient.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := firstClient.Ping(ctx).Err(); err != nil {
		t.Fatalf("connect to test Redis: %v", err)
	}
	prefix := "coderushoj-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	first, err := NewRedisQuotaFromClient(firstClient, prefix)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRedisQuotaFromClient(secondClient, prefix)
	if err != nil {
		t.Fatal(err)
	}
	request := QuotaRequest{
		TenantID: "tenant-atomic", Kind: QuotaJudgeSubmit, Cost: 1,
		Limit: QuotaLimit{Capacity: 8, RefillPeriod: time.Hour},
	}
	var allowed atomic.Int64
	errorsChannel := make(chan error, 16)
	var waitGroup sync.WaitGroup
	for index := 0; index < 16; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			quota := first
			if index%2 == 1 {
				quota = second
			}
			decision, err := quota.Allow(ctx, request)
			if err != nil {
				errorsChannel <- err
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}(index)
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatalf("concurrent quota error: %v", err)
	}
	if allowed.Load() != 8 {
		t.Fatalf("allowed=%d want=8", allowed.Load())
	}
	decision, err := first.Allow(ctx, request)
	if err != nil || decision.Allowed || decision.RetryAfter <= 0 {
		t.Fatalf("exhausted decision=%+v error=%v", decision, err)
	}
	uploadDecision, err := second.Allow(ctx, QuotaRequest{
		TenantID: request.TenantID, Kind: QuotaBundleUploadBytes, Cost: 1024,
		Limit: QuotaLimit{Capacity: 4096, RefillPeriod: time.Hour},
	})
	if err != nil || !uploadDecision.Allowed {
		t.Fatalf("isolated upload bucket decision=%+v error=%v", uploadDecision, err)
	}
	if err := firstClient.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Allow(context.Background(), request); !errors.Is(err, ErrQuotaUnavailable) {
		t.Fatalf("closed Redis error=%v", err)
	}
}

func TestRedisQuotaRefillsFromRedisTime(t *testing.T) {
	address := os.Getenv("REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("REDIS_TEST_ADDR is not configured")
	}
	client := redis.NewClient(&redis.Options{Addr: address})
	t.Cleanup(func() { _ = client.Close() })
	quota, err := NewRedisQuotaFromClient(client, "coderushoj-refill-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err != nil {
		t.Fatal(err)
	}
	request := QuotaRequest{
		TenantID: "tenant-refill", Kind: QuotaJudgeSubmit, Cost: 2,
		Limit: QuotaLimit{Capacity: 2, RefillPeriod: 200 * time.Millisecond},
	}
	if decision, err := quota.Allow(context.Background(), request); err != nil || !decision.Allowed {
		t.Fatalf("initial decision=%+v error=%v", decision, err)
	}
	request.Cost = 1
	deadline := time.Now().Add(2 * time.Second)
	for {
		decision, err := quota.Allow(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Allowed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bucket did not refill from Redis TIME: %+v", decision)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
