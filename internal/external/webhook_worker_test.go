package external

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"
)

type webhookRepositoryStub struct {
	claim       WebhookClaim
	claimErr    error
	settleErr   error
	sweepErr    error
	claimHook   func()
	sweepCounts []int64
	claims      int
	sweeps      int
	settlements []WebhookSettlement
}

func (repository *webhookRepositoryStub) SweepTerminal(context.Context, time.Duration, int) (int64, error) {
	repository.sweeps++
	if len(repository.sweepCounts) > 0 {
		count := repository.sweepCounts[0]
		repository.sweepCounts = repository.sweepCounts[1:]
		return count, repository.sweepErr
	}
	return 0, repository.sweepErr
}

func (repository *webhookRepositoryStub) ClaimNextWebhook(context.Context, string, time.Duration) (WebhookClaim, error) {
	repository.claims++
	if repository.claimHook != nil {
		repository.claimHook()
	}
	return repository.claim, repository.claimErr
}

func (repository *webhookRepositoryStub) SettleWebhook(_ context.Context, _ WebhookClaim, settlement WebhookSettlement) error {
	repository.settlements = append(repository.settlements, settlement)
	return repository.settleErr
}

type callbackDecryptorStub struct {
	secret    []byte
	encrypted EncryptedCallbackSecret
	err       error
	calls     int
}

func (decryptor *callbackDecryptorStub) Decrypt(_ string, _ string, _ string, encrypted EncryptedCallbackSecret) ([]byte, error) {
	decryptor.calls++
	decryptor.encrypted = encrypted
	return decryptor.secret, decryptor.err
}

type webhookOutcomeDeliverer struct {
	outcome WebhookOutcome
	block   bool
	secret  []byte
	calls   int
	started chan struct{}
}

type webhookContextProbeDeliverer struct{}

func (webhookContextProbeDeliverer) DeliverOutcome(ctx context.Context, _ WebhookDelivery, _ time.Duration) WebhookOutcome {
	if err := ctx.Err(); err != nil {
		return WebhookOutcome{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, err: err}
	}
	return WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: http.StatusNoContent}
}

func (deliverer *webhookOutcomeDeliverer) DeliverOutcome(ctx context.Context, delivery WebhookDelivery, _ time.Duration) WebhookOutcome {
	deliverer.calls++
	deliverer.secret = delivery.Secret
	if deliverer.block {
		if deliverer.started != nil {
			close(deliverer.started)
		}
		<-ctx.Done()
		return WebhookOutcome{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork, err: ctx.Err()}
	}
	return deliverer.outcome
}

type closeRecorder struct {
	mu     sync.Mutex
	closed int
}

func (recorder *closeRecorder) Close() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.closed++
	return nil
}

func (recorder *closeRecorder) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.closed
}

type delivererFactoryStub struct {
	deliverer webhookOutcomeClient
	closers   []*closeRecorder
}

type concurrentDelivererFactory struct {
	mu      sync.Mutex
	calls   int
	ready   chan struct{}
	release chan struct{}
	closers []*closeRecorder
}

func (factory *concurrentDelivererFactory) New(string, uint16, time.Duration, time.Duration) (webhookOutcomeClient, io.Closer, error) {
	factory.mu.Lock()
	factory.calls++
	deliverer := &webhookOutcomeDeliverer{}
	closer := &closeRecorder{}
	factory.closers = append(factory.closers, closer)
	if factory.calls == 2 {
		close(factory.ready)
	}
	factory.mu.Unlock()
	<-factory.release
	return deliverer, closer, nil
}

func (factory *delivererFactoryStub) New(string, uint16, time.Duration, time.Duration) (webhookOutcomeClient, io.Closer, error) {
	closer := &closeRecorder{}
	factory.closers = append(factory.closers, closer)
	return factory.deliverer, closer, nil
}

func TestWebhookBackoffUsesExponentialJitterCapsAndRetryAfter(t *testing.T) {
	base := 5 * time.Second
	maximum := 15 * time.Minute
	tests := []struct {
		name       string
		attempt    uint
		random     []byte
		retryAfter time.Duration
		wantMin    time.Duration
		wantMax    time.Duration
	}{
		{name: "attempt one lower jitter", attempt: 1, random: make([]byte, 8), wantMin: 2500 * time.Millisecond, wantMax: 2500 * time.Millisecond},
		{name: "attempt two lower jitter", attempt: 2, random: make([]byte, 8), wantMin: 5 * time.Second, wantMax: 5 * time.Second},
		{name: "upper jitter", attempt: 1, random: bytes.Repeat([]byte{0xff}, 8), wantMin: 7499 * time.Millisecond, wantMax: 7500 * time.Millisecond},
		{name: "saturates before shift", attempt: math.MaxUint, random: bytes.Repeat([]byte{0xff}, 8), wantMin: maximum, wantMax: maximum},
		{name: "retry after wins", attempt: 1, random: make([]byte, 8), retryAfter: 10 * time.Minute, wantMin: 10 * time.Minute, wantMax: 10 * time.Minute},
		{name: "retry after is capped", attempt: 1, random: make([]byte, 8), retryAfter: 24 * time.Hour, wantMin: maximum, wantMax: maximum},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			delay, err := webhookBackoff(base, maximum, test.attempt, test.retryAfter, bytes.NewReader(test.random))
			if err != nil || delay < test.wantMin || delay > test.wantMax || delay < 0 || delay > maximum {
				t.Fatalf("delay=%s err=%v want=[%s,%s]", delay, err, test.wantMin, test.wantMax)
			}
		})
	}
}

func TestWebhookBackoffRejectsEntropyFailure(t *testing.T) {
	if _, err := webhookBackoff(5*time.Second, 15*time.Minute, 1, 0, bytes.NewReader(nil)); err == nil {
		t.Fatal("expected entropy failure")
	}
}

func TestWebhookWorkerDefaultsAndRejectsLeaseShorterThanHTTP(t *testing.T) {
	config := WebhookWorkerConfig{
		Repository: &webhookRepositoryStub{}, CallbackCipher: &CallbackCipher{}, WorkerID: "webhook-worker-a",
	}
	worker, err := newWebhookWorker(config, &callbackDecryptorStub{}, &delivererFactoryStub{})
	if err != nil {
		t.Fatal(err)
	}
	if worker.leaseDuration != time.Minute || worker.requestTimeout != 30*time.Second || worker.baseRetryDelay != 5*time.Second ||
		worker.maximumRetry != 15*time.Minute || worker.maximumAttempts != 12 || worker.terminalRetention != 30*24*time.Hour {
		t.Fatalf("defaults were not applied: %+v", worker)
	}
	config.LeaseDuration = 10 * time.Second
	config.RequestTimeout = 10 * time.Second
	if _, err := newWebhookWorker(config, &callbackDecryptorStub{}, &delivererFactoryStub{}); err == nil {
		t.Fatal("lease equal to HTTP timeout was accepted")
	}
}

func TestWebhookWorkerClaimDecryptDeliverSettleAndClearsSecret(t *testing.T) {
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	claim := validWebhookWorkerClaim(now)
	repository := &webhookRepositoryStub{claim: claim}
	secret := bytes.Repeat([]byte{0x5a}, 32)
	decryptor := &callbackDecryptorStub{secret: secret}
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: http.StatusNoContent}}
	factory := &delivererFactoryStub{deliverer: deliverer}
	worker := newWorkerForTest(t, repository, decryptor, factory, now, bytes.NewReader(make([]byte, 64)), 2)

	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if decryptor.calls != 1 || deliverer.calls != 1 || len(repository.settlements) != 1 {
		t.Fatalf("decrypt=%d deliver=%d settlements=%d", decryptor.calls, deliverer.calls, len(repository.settlements))
	}
	settlement := repository.settlements[0]
	if settlement.Disposition != WebhookDelivered || settlement.HTTPStatus != http.StatusNoContent {
		t.Fatalf("settlement=%+v", settlement)
	}
	for _, value := range secret {
		if value != 0 {
			t.Fatal("decrypted secret was not cleared")
		}
	}
	if len(deliverer.secret) != 32 || !bytes.Equal(deliverer.secret, make([]byte, 32)) {
		t.Fatal("deliverer retained uncleared secret")
	}
	if !bytes.Equal(decryptor.encrypted.Ciphertext, make([]byte, 48)) || !bytes.Equal(decryptor.encrypted.Nonce, make([]byte, 12)) {
		t.Fatal("encrypted secret copies were not cleared")
	}
}

func TestWebhookWorkerClearsPartialPlaintextReturnedWithDecryptError(t *testing.T) {
	now := time.Now().UTC()
	partial := bytes.Repeat([]byte{0x6a}, 32)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(now)}
	worker := newWorkerForTest(t, repository,
		&callbackDecryptorStub{secret: partial, err: ErrCallbackEncryption},
		&delivererFactoryStub{deliverer: &webhookOutcomeDeliverer{}},
		now, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(partial, make([]byte, len(partial))) {
		t.Fatal("partial plaintext from failed decrypt was not cleared")
	}
}

func TestWebhookWorkerClassifiesUnsafeAndDecryptFailuresAsDead(t *testing.T) {
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		mutate     func(*WebhookClaim)
		decryptErr error
		wantCode   string
	}{
		{name: "unsafe authority", mutate: func(claim *WebhookClaim) { claim.AllowedHost = "other.example.com" }, wantCode: WebhookErrorUnsafeDestination},
		{name: "decrypt", mutate: func(*WebhookClaim) {}, decryptErr: ErrCallbackEncryption, wantCode: WebhookErrorCallbackDecrypt},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := validWebhookWorkerClaim(now)
			test.mutate(&claim)
			repository := &webhookRepositoryStub{claim: claim}
			decryptor := &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32), err: test.decryptErr}
			deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: 204}}
			worker := newWorkerForTest(t, repository, decryptor, &delivererFactoryStub{deliverer: deliverer}, now, bytes.NewReader(make([]byte, 8)), 2)
			if err := worker.processNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookPermanentFailure || repository.settlements[0].ErrorCode != test.wantCode {
				t.Fatalf("settlements=%+v", repository.settlements)
			}
			if deliverer.calls != 0 {
				t.Fatal("unsafe/decrypt-failed claim was delivered")
			}
		})
	}
}

func TestWebhookWorkerRetriesNetworkWithLaterOfBackoffAndRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(now)}
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookRetry, HTTPStatus: 429, ErrorCode: WebhookErrorHTTPRetryable, RetryAfter: 10 * time.Minute}}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: deliverer}, now, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookRetry || repository.settlements[0].RetryDelay != 10*time.Minute {
		t.Fatalf("settlements=%+v want retry delay=%s", repository.settlements, 10*time.Minute)
	}
}

func TestWebhookWorkerNetworkFailureReturnsToPending(t *testing.T) {
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(now)}
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork}}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: deliverer}, now, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookRetry ||
		repository.settlements[0].ErrorCode != WebhookErrorNetwork || repository.settlements[0].RetryDelay != 2500*time.Millisecond {
		t.Fatalf("settlements=%+v", repository.settlements)
	}
}

func TestWebhookWorkerSchedulesFromClaimDatabaseClockNotApplicationClock(t *testing.T) {
	databaseNow := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	applicationNow := databaseNow.Add(180 * 24 * time.Hour)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(databaseNow)}
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork}}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: deliverer}, applicationNow, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.settlements) != 1 || repository.settlements[0].RetryDelay != 2500*time.Millisecond {
		t.Fatalf("retry delay=%s want=%s", repository.settlements[0].RetryDelay, 2500*time.Millisecond)
	}
}

func TestWebhookWorkerAccountsForClaimRoundTripWhenComputingRemainingLease(t *testing.T) {
	databaseNow := time.Now().UTC()
	localNow := databaseNow.Add(180 * 24 * time.Hour)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(databaseNow)}
	repository.claimHook = func() { localNow = localNow.Add(40 * time.Second) }
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: 204}}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)},
		&delivererFactoryStub{deliverer: deliverer}, localNow, bytes.NewReader(make([]byte, 8)), 2)
	worker.now = func() time.Time { return localNow }
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookDelivered {
		t.Fatalf("delayed claim lost a still-live lease: %+v", repository.settlements)
	}
}

func TestWebhookWorkerMonotonicClaimElapsedSurvivesWallClockRollback(t *testing.T) {
	databaseNow := time.Now().UTC()
	wallNow := databaseNow.Add(180 * 24 * time.Hour)
	monotonicElapsed := time.Duration(0)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(databaseNow)}
	repository.claimHook = func() {
		wallNow = wallNow.Add(-6 * time.Hour)
		monotonicElapsed = 70 * time.Second
	}
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: 204}}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)},
		&delivererFactoryStub{deliverer: deliverer}, wallNow, bytes.NewReader(make([]byte, 8)), 2)
	worker.now = func() time.Time { return wallNow }
	worker.elapsedSince = func(time.Time) time.Duration { return monotonicElapsed }
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 0 || len(repository.settlements) != 0 {
		t.Fatalf("expired local lease budget delivered=%d settlements=%+v", deliverer.calls, repository.settlements)
	}
}

func TestWebhookWorkerUsesLeaseDurationInsteadOfDatabaseWallClockForHTTPContext(t *testing.T) {
	databaseNow := time.Now().UTC().Add(-180 * 24 * time.Hour)
	applicationNow := databaseNow.Add(365 * 24 * time.Hour)
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(databaseNow)}
	worker := newWorkerForTest(t, repository,
		&callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)},
		&delivererFactoryStub{deliverer: webhookContextProbeDeliverer{}},
		applicationNow, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookDelivered {
		t.Fatalf("database wall-clock skew cancelled live delivery: %+v", repository.settlements)
	}
}

func TestWebhookWorkerExhaustionOrDeadlineAsksRepositoryForTerminalRetrySettlement(t *testing.T) {
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	for _, mutate := range []func(*WebhookClaim){
		func(claim *WebhookClaim) { claim.AttemptCount = 12 },
		func(claim *WebhookClaim) { claim.ExpiresAt = now.Add(2 * time.Second) },
	} {
		claim := validWebhookWorkerClaim(now)
		mutate(&claim)
		repository := &webhookRepositoryStub{claim: claim}
		deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookRetry, ErrorCode: WebhookErrorNetwork}}
		worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: deliverer}, now, bytes.NewReader(make([]byte, 8)), 2)
		if err := worker.processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookRetry || repository.settlements[0].ErrorCode != WebhookErrorNetwork || repository.settlements[0].RetryDelay < 2500*time.Millisecond {
			t.Fatalf("settlements=%+v", repository.settlements)
		}
	}
}

func TestWebhookWorkerIgnoresStaleSettlementOwnershipLoss(t *testing.T) {
	now := time.Now().UTC()
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(now), settleErr: ErrWebhookLeaseLost}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: 200}}}, now, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatalf("stale ownership should be ignored: %v", err)
	}
}

func TestWebhookWorkerNeverDeliversExpiredClaim(t *testing.T) {
	now := time.Now().UTC()
	claim := validWebhookWorkerClaim(now)
	claim.ExpiresAt = now
	repository := &webhookRepositoryStub{claim: claim}
	deliverer := &webhookOutcomeDeliverer{}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: deliverer}, now, bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.processNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 0 || len(repository.settlements) != 1 || repository.settlements[0].Disposition != WebhookPermanentFailure || repository.settlements[0].ErrorCode != WebhookErrorDeliveryExpired {
		t.Fatalf("deliver=%d settlements=%+v", deliverer.calls, repository.settlements)
	}
	if !validWebhookSettlement(repository.settlements[0]) {
		t.Fatal("delivery-expired settlement was rejected by repository validation")
	}
}

func TestWebhookWorkerCancellationInterruptsDelivery(t *testing.T) {
	now := time.Now().UTC()
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(now)}
	deliverer := &webhookOutcomeDeliverer{block: true, started: make(chan struct{})}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: deliverer}, now, bytes.NewReader(make([]byte, 8)), 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.processNext(ctx) }()
	<-deliverer.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(repository.settlements) != 0 {
		t.Fatal("cancelled delivery must recover through lease, not settle")
	}
}

func TestWebhookWorkerRunIdlesWithContextAndReturnsRepositoryError(t *testing.T) {
	worker := newWorkerForTest(t, &webhookRepositoryStub{claimErr: ErrWebhookNotAvailable}, &callbackDecryptorStub{}, &delivererFactoryStub{}, time.Now().UTC(), bytes.NewReader(make([]byte, 8)), 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled idle error=%v", err)
	}
	want := errors.New("database unavailable")
	worker = newWorkerForTest(t, &webhookRepositoryStub{claimErr: want}, &callbackDecryptorStub{}, &delivererFactoryStub{}, time.Now().UTC(), bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("repository error=%v", err)
	}
	repository := &webhookRepositoryStub{claim: validWebhookWorkerClaim(time.Now().UTC()), settleErr: want}
	worker = newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, &delivererFactoryStub{deliverer: &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: 204}}}, time.Now().UTC(), bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("settlement repository error=%v", err)
	}
	worker = newWorkerForTest(t, &webhookRepositoryStub{sweepErr: want}, &callbackDecryptorStub{}, &delivererFactoryStub{}, time.Now().UTC(), bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("retention repository error=%v", err)
	}
}

func TestWebhookWorkerDrainsRetentionBatchesBeforeHourlyDelay(t *testing.T) {
	stop := errors.New("stop after retention")
	repository := &webhookRepositoryStub{
		claimErr: stop, sweepCounts: []int64{webhookRetentionSweepBatch, webhookRetentionSweepBatch, 17},
	}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{}, &delivererFactoryStub{}, time.Now().UTC(), bytes.NewReader(make([]byte, 8)), 2)
	if err := worker.Run(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("run error=%v", err)
	}
	if repository.sweeps != 3 {
		t.Fatalf("retention sweeps=%d want=3", repository.sweeps)
	}
}

func TestWebhookWorkerAuthorityCacheEvictsAndClosesIdleConnections(t *testing.T) {
	now := time.Now().UTC()
	repository := &webhookRepositoryStub{}
	deliverer := &webhookOutcomeDeliverer{outcome: WebhookOutcome{Disposition: WebhookDelivered, HTTPStatus: 204}}
	factory := &delivererFactoryStub{deliverer: deliverer}
	worker := newWorkerForTest(t, repository, &callbackDecryptorStub{secret: bytes.Repeat([]byte{1}, 32)}, factory, now, bytes.NewReader(make([]byte, 32)), 1)
	for index, host := range []string{"one.example.com", "two.example.com"} {
		claim := validWebhookWorkerClaim(now)
		claim.AllowedHost = host
		claim.DestinationURL = "https://" + host + "/hook"
		repository.claim = claim
		if err := worker.processNext(context.Background()); err != nil {
			t.Fatal(err)
		}
		if index == 1 && factory.closers[0].count() != 1 {
			t.Fatal("evicted authority did not close idle connections")
		}
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if factory.closers[1].count() != 1 {
		t.Fatal("worker close did not close cached authority")
	}
}

func TestWebhookWorkerConcurrentAuthorityMissReturnsOneLiveDeliverer(t *testing.T) {
	factory := &concurrentDelivererFactory{ready: make(chan struct{}), release: make(chan struct{})}
	worker := newWorkerForTest(t, &webhookRepositoryStub{}, &callbackDecryptorStub{}, factory, time.Now().UTC(), bytes.NewReader(make([]byte, 8)), 2)
	results := make(chan webhookOutcomeClient, 2)
	errorsChannel := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func() {
			deliverer, err := worker.cachedDeliverer("same.example.com", 443)
			results <- deliverer
			errorsChannel <- err
		}()
	}
	<-factory.ready
	close(factory.release)
	first, second := <-results, <-results
	if err := <-errorsChannel; err != nil {
		t.Fatal(err)
	}
	if err := <-errorsChannel; err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("concurrent cache miss returned different deliverers")
	}
	closedBeforeWorkerClose := factory.closers[0].count() + factory.closers[1].count()
	if closedBeforeWorkerClose != 1 {
		t.Fatalf("duplicate authority closers closed=%d want=1", closedBeforeWorkerClose)
	}
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if got := factory.closers[0].count() + factory.closers[1].count(); got != 2 {
		t.Fatalf("all authority closers closed=%d want=2", got)
	}
}

func validWebhookWorkerClaim(now time.Time) WebhookClaim {
	return WebhookClaim{
		OutboxID: 1, EventID: "eventabcdefghijklmnopqrstu", EventType: "judge.job.completed",
		TenantID: "tenantabcdefghijklmnopqrst", CallbackID: "callbackabcdefghijklmnopqr",
		DestinationURL: "https://hooks.example.com/hook", AllowedHost: "hooks.example.com", AllowedPort: 443,
		Body: []byte(`{"schemaVersion":1,"eventId":"eventabcdefghijklmnopqrstu"}`), AttemptCount: 1,
		WorkerID: "webhook-worker-a", LeaseToken: bytes.Repeat([]byte{1}, 32), LeaseUntil: now.Add(time.Minute), ExpiresAt: now.Add(24 * time.Hour),
		secret: EncryptedCallbackSecret{Ciphertext: bytes.Repeat([]byte{2}, 48), Nonce: bytes.Repeat([]byte{3}, 12), KeyVersion: 1},
	}
}

func newWorkerForTest(t *testing.T, repository webhookOutboxRepository, decryptor callbackSecretDecryptor, factory webhookDelivererFactory, now time.Time, random io.Reader, cacheSize int) *WebhookWorker {
	t.Helper()
	worker, err := newWebhookWorker(WebhookWorkerConfig{
		Repository: repository, WorkerID: "webhook-worker-a", LeaseDuration: time.Minute,
		RequestTimeout: 30 * time.Second, DialTimeout: 5 * time.Second,
		BaseRetryDelay: 5 * time.Second, MaximumRetryDelay: 15 * time.Minute,
		MaximumAttempts: 12, IdleDelay: time.Minute, TerminalRetention: 30 * 24 * time.Hour,
		MaximumAuthorities: cacheSize, Random: random, Now: func() time.Time { return now },
	}, decryptor, factory)
	if err != nil {
		t.Fatal(err)
	}
	worker.elapsedSince = func(start time.Time) time.Duration { return worker.now().Sub(start) }
	return worker
}
